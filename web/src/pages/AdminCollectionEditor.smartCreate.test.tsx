import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router";
import { beforeEach, describe, expect, it, vi } from "vitest";

import AdminCollectionEditor from "./AdminCollectionEditor";

const mocks = vi.hoisted(() => ({ v2: vi.fn() }));

vi.mock("@/api/v2/request", async () => {
  const actual = await vi.importActual<typeof import("@/api/v2/request")>("@/api/v2/request");
  return {
    ...actual,
    v2: (operation: string, options?: unknown) => mocks.v2(operation, options),
  };
});
vi.mock("@/hooks/queries/admin/libraries", () => ({
  useAdminLibraries: () => ({ data: [{ id: 7, name: "Movies" }] }),
}));
vi.mock("@/hooks/queries/catalog", () => ({
  useCatalogWindow: () => ({ data: { totalItems: 3, pages: new Map() }, isLoading: false }),
}));
vi.mock("@/hooks/queries/libraries", () => ({ useUserLibraries: () => ({ data: [] }) }));
vi.mock("@/components/catalog/CatalogFiltersPanel", () => ({ default: () => null }));
vi.mock("@/components/ItemGrid", () => ({ default: () => null }));
vi.mock("@/components/CollectionTemplateGallery", () => ({
  CollectionTemplateGallery: () => null,
}));
vi.mock("@/components/ImageUploadField", () => ({ ImageUploadField: () => null }));
vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn(), warning: vi.fn() },
}));

class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.stubGlobal("ResizeObserver", ResizeObserverStub);
  mocks.v2.mockImplementation((operation: string) => {
    if (operation === "GET /api/v2/admin/collections") {
      return Promise.resolve({ items: [], groups: [] });
    }
    if (operation === "GET /api/v2/admin/collections/capabilities") {
      return Promise.resolve({ artwork: false, imports: true, groups: true, item_reorder: true });
    }
    if (operation === "POST /api/v2/admin/collections") {
      return Promise.resolve({
        id: "ac1",
        title: "New Smart Collection",
        collection_type: "smart",
        library_id: "7",
        library_ids: ["7"],
      });
    }
    return Promise.reject(new Error(`unexpected operation: ${operation}`));
  });
});

function showEditor() {
  render(
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter initialEntries={["/admin/collections/new?libraryId=7"]}>
        <Routes>
          <Route path="/admin/collections/new" element={<AdminCollectionEditor />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("admin smart collection create", () => {
  it("creates a smart collection from the Smart tile through the wizard", async () => {
    showEditor();

    // The Smart tile on the source picker opens the wizard directly.
    fireEvent.click(
      screen.getByRole("button", {
        name: /Smart Match titles with filters that stay up to date/,
      }),
    );
    expect(await screen.findByRole("heading", { name: "New Collection" })).toBeInTheDocument();

    // The URL's library seeds the draft, so the wizard can advance.
    fireEvent.click(screen.getByRole("button", { name: "Next: Details" }));
    fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Late sci-fi" } });
    fireEvent.click(screen.getByRole("button", { name: "Create Collection" }));

    await waitFor(() =>
      expect(mocks.v2).toHaveBeenCalledWith("POST /api/v2/admin/collections", expect.anything()),
    );
    const createCall = mocks.v2.mock.calls.find(
      ([operation]) => operation === "POST /api/v2/admin/collections",
    );
    expect(createCall?.[1]).toMatchObject({
      body: {
        title: "Late sci-fi",
        collection_type: "smart",
        library_ids: ["7"],
        query_definition: expect.objectContaining({ library_ids: [7] }),
      },
    });
  });
});
