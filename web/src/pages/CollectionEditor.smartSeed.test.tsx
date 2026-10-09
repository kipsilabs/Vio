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
  library_id: "3",
  "groups[0][match]": "all",
  "groups[0][rules][0][field]": "genre",
  "groups[0][rules][0][op]": "contains",
  "groups[0][rules][0][value]": "Action",
});

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
    const parsed = parseCatalogSearchParams(new URLSearchParams(seededHref().split("?")[1]));
    expect(parsed.source).toBe("query");
    expect(parsed.query_definition.library_ids).toEqual([3]);
    expect(parsed.query_definition.groups).toEqual([
      { match: "all", rules: [{ field: "genre", op: "contains", value: "Action" }] },
    ]);
  });

  it("seeds the smart wizard with the current filters and saves via POST /api/v2/collections", async () => {
    showSeededEditor();

    // Step 1 previews the seeded query definition.
    expect(await screen.findByRole("heading", { name: "New Collection" })).toBeInTheDocument();
    expect(mocks.useCatalogWindow).toHaveBeenCalledWith(
      expect.objectContaining({
        source: "query",
        query_definition: expect.objectContaining({
          library_ids: [3],
          groups: [{ match: "all", rules: [{ field: "genre", op: "contains", value: "Action" }] }],
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
          library_ids: [3],
          groups: [{ match: "all", rules: [{ field: "genre", op: "contains", value: "Action" }] }],
        }),
      },
    });
  });
});
