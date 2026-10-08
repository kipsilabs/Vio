/**
 * Goldens: the requests today's collection forms send to create and update
 * manual and smart collections, admin and personal, through the editor pages.
 * Later editor work changes a golden here only on purpose.
 */
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router";
import { toast } from "sonner";
import { beforeEach, describe, expect, it, vi } from "vitest";

import getCollectionOk from "../../../contracts/api/v2/fixtures/get_collection_ok.json";
import {
  adminCapabilities,
  adminCollection,
  adminCollectionList,
  adminSmartCollection,
  emptyPreview,
  personalCapabilities,
  personalSmartCollection,
} from "@/test/fixtures/collectionAnswers";
import { goldens } from "@/test/fixtures/collectionBodies";
import { installV2Recorder, v2Recorder } from "@/test/v2Recorder";
import AdminCollectionEditor from "./AdminCollectionEditor";
import CollectionEditor from "./CollectionEditor";

vi.mock("@/api/v2/request", async () => (await import("@/test/v2Recorder")).mockV2Request());
vi.mock("@/hooks/queries/profiles", () => ({
  useProfiles: () => ({ data: [{ id: "p-owner", name: "Owner" }] }),
}));
vi.mock("@/hooks/useCurrentProfile", () => ({
  useCurrentProfile: () => ({ profile: { id: "p-owner" } }),
}));
vi.mock("@/hooks/queries/admin/libraries", () => ({
  useAdminLibraries: () => ({ data: [{ id: 1, name: "Movies", type: "movies" }] }),
}));
vi.mock("@/hooks/queries/libraries", async () => ({
  ...(await vi.importActual<typeof import("@/hooks/queries/libraries")>(
    "@/hooks/queries/libraries",
  )),
  useUserLibraries: () => ({ data: [{ id: 1, name: "Movies", type: "movies" }] }),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), warning: vi.fn(), error: vi.fn() } }));

installV2Recorder();

beforeEach(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  URL.createObjectURL = () => "blob:poster";
  HTMLElement.prototype.scrollIntoView = () => {};
  HTMLElement.prototype.hasPointerCapture = () => false;
  v2Recorder.answer("GET /api/v2/admin/collections/capabilities", adminCapabilities);
  v2Recorder.answer("GET /api/v2/collections/capabilities", personalCapabilities);
  v2Recorder.answer("GET /api/v2/admin/collections/{id}", adminCollection);
  v2Recorder.answer("GET /api/v2/admin/collections", adminCollectionList(adminCollection));
  v2Recorder.answer("POST /api/v2/admin/collections/preview", emptyPreview);
  v2Recorder.answer("POST /api/v2/collections/preview", emptyPreview);
  v2Recorder.answer("GET /api/v2/collections/{id}/items/order", {
    ordered_ids: ["movie:heat-1995"],
    has_more: false,
  });
  v2Recorder.answer("GET /api/v2/admin/collections/{id}/items", {
    items: [],
    page: { has_more: false },
  });
  v2Recorder.answer("GET /api/v2/admin/collections/{id}/items/order", {
    ordered_ids: [],
    has_more: false,
  });
});

function showPage(url: string) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[url]}>
        <Routes>
          <Route path="/admin/collections" element={<p>Admin collections page</p>} />
          <Route path="/admin/collections/new" element={<AdminCollectionEditor />} />
          <Route path="/admin/collections/:id/edit" element={<AdminCollectionEditor />} />
          <Route path="/collections" element={<p>Collections page</p>} />
          <Route path="/collections/new" element={<CollectionEditor />} />
          <Route path="/collections/:id/edit" element={<CollectionEditor />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

async function closedTo(page: "Admin collections page" | "Collections page") {
  await screen.findByText(page);
}

function artworkField(label: "Poster" | "Backdrop") {
  return screen.getByText(label, { selector: "label" }).parentElement!;
}

async function artworkShown(label: "Poster" | "Backdrop") {
  await screen.findByText(label, { selector: "label" });
}

function chooseArtworkFile(label: "Poster" | "Backdrop", name: string) {
  fireEvent.change(artworkField(label).querySelector("input[type=file]")!, {
    target: { files: [new File(["image"], name, { type: "image/png" })] },
  });
}

function pasteArtworkURL(label: "Poster" | "Backdrop", url: string) {
  fireEvent.change(within(artworkField(label)).getByRole("textbox"), { target: { value: url } });
}

function deleteArtwork(label: "Poster" | "Backdrop") {
  fireEvent.click(within(artworkField(label)).getByTitle("Delete image"));
}

async function pickOption(trigger: HTMLElement, option: string) {
  fireEvent.click(trigger);
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

function comboboxShowing(text: string) {
  const found = screen.getAllByRole("combobox").find((element) => element.textContent === text);
  if (!found) throw new Error(`No combobox shows "${text}"`);
  return found;
}

/** The goldens hold writes only; what a page reads around them may change freely. */
function writes() {
  return v2Recorder.writes();
}

describe("admin manual and smart collections", () => {
  it("creates a manual collection, then uploads its poster and backdrop in that order", async () => {
    showPage("/admin/collections/new?libraryId=1");
    fireEvent.click(await screen.findByRole("button", { name: /^Manual/ }));
    fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Staff picks" } });
    chooseArtworkFile("Poster", "poster.png");
    pasteArtworkURL("Backdrop", "https://images.example/backdrop.png");
    fireEvent.click(screen.getByRole("button", { name: "Save Collection" }));
    await closedTo("Admin collections page");
    expect(writes()).toEqual(goldens.adminManualCreate);
  });

  it("creates a smart collection with its rules and sort", async () => {
    showPage("/admin/collections/new?libraryId=1");
    fireEvent.click(await screen.findByRole("button", { name: /^Manual/ }));
    fireEvent.change(screen.getByLabelText("Title"), { target: { value: "New this month" } });
    await pickOption(comboboxShowing("Manual"), "Smart");
    fireEvent.click(screen.getByRole("button", { name: "Save Collection" }));
    await closedTo("Admin collections page");
    expect(writes()).toEqual(goldens.adminSmartCreate);
  });

  it("updates a manual collection with featured and collection_type in the PATCH", async () => {
    showPage("/admin/collections/c1/edit?libraryId=1");
    fireEvent.change(await screen.findByLabelText("Title"), { target: { value: "Renamed" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Collection" }));
    await closedTo("Admin collections page");
    expect(writes()).toEqual(goldens.adminManualUpdate);
    expect(writes()[0]!.body).toHaveProperty("featured", false);
  });

  it("deletes a staged poster removal only after the PATCH succeeds", async () => {
    v2Recorder.answer("GET /api/v2/admin/collections", adminCollectionList(withArtwork()));
    showPage("/admin/collections/c1/edit?libraryId=1");
    await artworkShown("Poster");
    deleteArtwork("Poster");
    await act(async () => {});
    expect(writes()).toEqual([]);
    fireEvent.click(screen.getByRole("button", { name: "Save Collection" }));
    await closedTo("Admin collections page");
    expect(writes()).toEqual(goldens.adminStagedPosterRemoval);
  });

  it("replaces a removed backdrop with a file without deleting it", async () => {
    v2Recorder.answer("GET /api/v2/admin/collections", adminCollectionList(withArtwork()));
    showPage("/admin/collections/c1/edit?libraryId=1");
    await artworkShown("Backdrop");
    deleteArtwork("Backdrop");
    chooseArtworkFile("Backdrop", "backdrop.png");
    fireEvent.click(screen.getByRole("button", { name: "Save Collection" }));
    await closedTo("Admin collections page");
    expect(writes()).toEqual(goldens.adminBackdropReplacement);
  });
});

function withArtwork() {
  return {
    ...adminCollection,
    poster_url: "https://images.example/poster.png",
    backdrop_url: "https://images.example/backdrop.png",
  };
}

describe("smart collections saved unchanged", () => {
  const limits = [
    ["no limit", undefined],
    ["the server's no-limit sentinel", 10_000_000],
    ["a limit of 250", 250],
  ] as const;

  it.each(limits)("admin: keeps %s", async (label, limit) => {
    v2Recorder.answer(
      "GET /api/v2/admin/collections/{id}",
      adminSmartCollection(storedQuery(limit)),
    );
    v2Recorder.answer("POST /api/v2/catalog/query", emptyCatalogPage);
    showPage("/admin/collections/c1/edit?libraryId=1");
    fireEvent.click(await screen.findByRole("button", { name: "Next: Details" }));
    fireEvent.click(screen.getByRole("button", { name: "Save Collection" }));
    await closedTo("Admin collections page");
    expect(writes()).toEqual(goldens.adminSmartUnchanged[label]);
    expect(writes()[0]!.body).toHaveProperty("featured", false);
  });

  it.each(limits)("personal: keeps %s", async (label, limit) => {
    v2Recorder.answer("GET /api/v2/collections/{id}", personalSmartCollection(storedQuery(limit)));
    v2Recorder.answer("POST /api/v2/catalog/query", emptyCatalogPage);
    showPage("/collections/c1/edit");
    fireEvent.click(await screen.findByRole("button", { name: "Next: Details" }));
    fireEvent.click(screen.getByRole("button", { name: "Save Collection" }));
    await closedTo("Collections page");
    expect(writes()).toEqual(goldens.personalSmartUnchanged[label]);
    expect(writes()[0]!.body).not.toHaveProperty("description");
  });
});

const emptyCatalogPage = {
  items: [],
  page: { has_more: false },
  total: 0,
  total_exact: true,
  effective_sort: { field: "added_at", order: "desc" },
};

function storedQuery(limit: number | undefined) {
  return {
    library_ids: [1],
    match: "all",
    groups: [{ match: "all", rules: [{ field: "genre", op: "is", value: "Comedy" }] }],
    sort: { field: "added_at", order: "desc" },
    ...(limit === undefined ? {} : { limit }),
  };
}

describe("personal manual and smart collections", () => {
  it("creates a smart collection by default, then uploads its poster", async () => {
    showPage("/collections/new");
    fireEvent.change(await screen.findByLabelText("Name"), { target: { value: "Comfort" } });
    chooseArtworkFile("Poster", "poster.png");
    fireEvent.click(screen.getByRole("button", { name: "Save Collection" }));
    await closedTo("Collections page");
    expect(writes()).toEqual(goldens.personalSmartCreate);
  });

  it("creates a manual collection with a pasted poster URL in the POST body", async () => {
    showPage("/collections/new");
    fireEvent.change(await screen.findByLabelText("Name"), { target: { value: "Rainy days" } });
    await pickOption(comboboxShowing("Smart"), "Manual");
    pasteArtworkURL("Poster", "https://images.example/poster.png");
    fireEvent.click(screen.getByRole("button", { name: "Save Collection" }));
    await closedTo("Collections page");
    expect(writes()).toEqual(goldens.personalManualCreate);
  });

  it("updates a manual collection without a description", async () => {
    showPage("/collections/c1/edit");
    fireEvent.change(await screen.findByLabelText("Name"), { target: { value: "Renamed" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Collection" }));
    await closedTo("Collections page");
    expect(writes()).toEqual(goldens.personalManualUpdate);
    expect(writes()[0]!.body).not.toHaveProperty("description");
  });

  it("deletes a removed poster at once, before Save", async () => {
    v2Recorder.answer("GET /api/v2/collections", {
      items: [{ ...getCollectionOk, poster_url: "https://images.example/poster.png" }],
    });
    showPage("/collections/c1/edit");
    await artworkShown("Poster");
    deleteArtwork("Poster");
    await waitFor(() => expect(writes()).toHaveLength(1));
    expect(writes()).toEqual(goldens.personalPosterRemoval);
  });
});

describe("personal manual page: a title added before Save", () => {
  it("leaves the form's ETag behind, so Save answers 412", async () => {
    showPage("/collections/c1/edit");
    fireEvent.change(await screen.findByPlaceholderText("Search the catalog to add titles…"), {
      target: { value: "alien" },
    });
    // The first search hit is already in the collection; add the second.
    const [firstNew] = await screen.findAllByRole("button", { name: "Add" });
    fireEvent.click(firstNew!);
    await vi.waitFor(() => expect(writes()).toHaveLength(1));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Renamed" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Collection" }));
    await vi.waitFor(() => expect(writes()).toHaveLength(2));
    expect(writes()).toEqual(goldens.personalAddThenRename);
    expect(v2Recorder.etag("/api/v2/collections/c1")).toBe('"/api/v2/collections/c1#2"');
    await vi.waitFor(() =>
      expect(toast.error).toHaveBeenCalledWith(
        expect.stringContaining("This collection changed while you were editing."),
      ),
    );
    expect(screen.queryByText("Collections page")).toBeNull();
  });
});
