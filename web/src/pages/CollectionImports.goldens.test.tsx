/**
 * Goldens: the requests today's synced-list forms send, admin and personal:
 * the MDBList and TMDB import forms, both template forms (with an MDBList
 * search pick and a template poster), the admin source editor and the
 * personal synced-list editor. Imports default `featured` on today.
 */
import type { ReactElement } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { Library } from "@/api/types";
import { CollectionTemplateGallery } from "@/components/CollectionTemplateGallery";
import {
  adminCapabilities,
  adminCollection,
  adminCollectionList,
  adminSyncedCollection,
  personalCapabilities,
  personalSyncedCollection,
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
  useAdminLibraries: () => ({ data: libraries }),
}));
vi.mock("@/hooks/queries/libraries", async () => ({
  ...(await vi.importActual<typeof import("@/hooks/queries/libraries")>(
    "@/hooks/queries/libraries",
  )),
  useUserLibraries: () => ({ data: libraries }),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), warning: vi.fn(), error: vi.fn() } }));

const libraries = [{ id: 1, name: "Movies", type: "movies" }] as Library[];

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
  HTMLElement.prototype.scrollIntoView = () => {};
  HTMLElement.prototype.hasPointerCapture = () => false;
  v2Recorder.answer("GET /api/v2/admin/collections/capabilities", adminCapabilities);
  v2Recorder.answer("GET /api/v2/collections/capabilities", personalCapabilities);
  v2Recorder.answer("GET /api/v2/admin/collections", adminCollectionList(adminCollection));
  v2Recorder.answer("GET /api/v2/admin/collections/templates", templateCatalog);
  v2Recorder.answer("GET /api/v2/collections/templates", templateCatalog);
  v2Recorder.answer("GET /api/v2/admin/collections/template-bundles", { bundles: [] });
});

const templateCatalog = {
  categories: [
    {
      category: "trending",
      label: "Trending",
      templates: [
        {
          id: "tmdb_trending_movies_week",
          title: "Trending Movies This Week",
          description: "Top trending movies on TMDB.",
          icon: "🎬",
          category: "trending",
          source: "tmdb",
          media_kind: "movie",
          default_limit: 50,
          default_sync_schedule: "0 4 * * *",
          poster_path: "https://images.example/templates/trending-movies.jpg",
          tmdb: { preset: "trending", media_type: "movie", time_window: "week" },
        },
      ],
    },
    {
      category: "custom",
      label: "Custom",
      templates: [
        {
          id: "mdblist_custom",
          title: "Custom MDBList",
          description: "Any public MDBList list.",
          icon: "📋",
          category: "custom",
          source: "mdblist",
          media_kind: "mixed",
          mdblist: { url: "" },
        },
      ],
    },
  ],
};

function show(element: ReactElement, url = "/") {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[url]}>
        <Routes>
          <Route path="/" element={element} />
          <Route path="/admin/collections" element={<p>Admin collections page</p>} />
          <Route path="/admin/collections/new" element={<AdminCollectionEditor />} />
          <Route path="/admin/collections/:id/edit" element={<AdminCollectionEditor />} />
          <Route path="/collections" element={<p>Collections page</p>} />
          <Route path="/collections/:id/edit" element={<CollectionEditor />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function type(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

async function pickOption(trigger: HTMLElement, option: string) {
  fireEvent.click(trigger);
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

async function closedTo(page: "Admin collections page" | "Collections page") {
  await screen.findByText(page);
}

describe("admin import forms", () => {
  it("imports an MDBList list, featured by default", async () => {
    show(<></>, "/admin/collections/new?libraryId=1");
    fireEvent.click(await screen.findByRole("button", { name: /^MDBList/ }));
    type("Collection Title", "Top Watched");
    type("MDBList JSON URL", "https://mdblist.com/lists/user/top-watched/json");
    fireEvent.click(screen.getByRole("button", { name: "Import MDBList Collection" }));
    await closedTo("Admin collections page");
    expect(v2Recorder.writes()).toEqual(goldens.adminImportMDBList);
  });

  it("imports a TMDB chart", async () => {
    show(<></>, "/admin/collections/new?libraryId=1");
    fireEvent.click(await screen.findByRole("button", { name: /^TMDB/ }));
    type("Collection Title", "Trending Today");
    fireEvent.click(screen.getByRole("button", { name: "Import TMDB Collection" }));
    await closedTo("Admin collections page");
    expect(v2Recorder.writes()).toEqual(goldens.adminImportTMDBChart);
  });

  it("imports a TMDB list", async () => {
    show(<></>, "/admin/collections/new?libraryId=1");
    fireEvent.click(await screen.findByRole("button", { name: /^TMDB/ }));
    type("Collection Title", "Festival Picks");
    await pickOption(screen.getByLabelText("Source"), "Public list (themoviedb.org URL)");
    type("TMDB list URL", "https://www.themoviedb.org/list/310-festival-picks");
    fireEvent.click(screen.getByRole("button", { name: "Import TMDB List" }));
    await closedTo("Admin collections page");
    expect(v2Recorder.writes()).toEqual(goldens.adminImportTMDBList);
  });
});

describe("template forms", () => {
  it("admin: creates from a TMDB template with its server poster", async () => {
    show(
      <CollectionTemplateGallery
        open
        onOpenChange={vi.fn()}
        libraries={libraries}
        initialLibraryId={1}
      />,
    );
    fireEvent.click(await screen.findByRole("button", { name: /Trending Movies This Week/ }));
    expect(await screen.findByRole("radio", { name: "Server default" })).toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: "Create Collection" }));
    await screen.findByText("Browse Collection Templates");
    expect(v2Recorder.writes()).toEqual(goldens.adminTemplateTMDB);
  });

  it("admin: creates from a list picked in MDBList search", async () => {
    v2Recorder.answer("GET /api/v2/collections/import/mdblist/search", {
      configured: true,
      items: [mdblistSearchHit],
    });
    show(
      <CollectionTemplateGallery
        open
        onOpenChange={vi.fn()}
        libraries={libraries}
        initialLibraryId={1}
      />,
    );
    fireEvent.click(await screen.findByRole("button", { name: /Custom MDBList/ }));
    type("Search MDBList", "oscar");
    fireEvent.click(await screen.findByRole("button", { name: /Oscar Winners/ }));
    fireEvent.click(screen.getByRole("button", { name: "Create Collection" }));
    await screen.findByText("Browse Collection Templates");
    expect(v2Recorder.callsOf("GET /api/v2/collections/import/mdblist/search")).toEqual([
      expect.objectContaining({ query: { q: "oscar" } }),
    ]);
    expect(v2Recorder.writes()).toEqual(goldens.adminTemplateMDBListPick);
  });

  it("personal: creates from a TMDB template with its server poster", async () => {
    show(<CollectionTemplateGallery mode="user" open onOpenChange={vi.fn()} />);
    fireEvent.click(await screen.findByRole("button", { name: /Trending Movies This Week/ }));
    expect(await screen.findByRole("radio", { name: "Server default" })).toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: "Create Collection" }));
    await screen.findByText("Browse Collection Templates");
    expect(v2Recorder.writes()).toEqual(goldens.personalTemplateTMDB);
  });
});

const mdblistSearchHit = {
  id: "4242",
  user_id: "7",
  user_name: "cinephile",
  name: "Oscar Winners",
  slug: "oscar-winners",
  description: "Best Picture winners.",
  media_type: "movie",
  items: 96,
  likes: 12,
  url: "https://mdblist.com/lists/cinephile/oscar-winners",
};

describe("admin source editor", () => {
  it("sends the whole MDBList source_config with a changed limit", async () => {
    v2Recorder.answer(
      "GET /api/v2/admin/collections/{id}",
      adminSyncedCollection("mdblist", {
        source_url: "https://mdblist.com/lists/user/top-watched/json",
        source_config: {
          mode: "mdblist_json",
          url: "https://mdblist.com/lists/user/top-watched/json",
          limit: 50,
        },
      }),
    );
    show(<></>, "/admin/collections/c1/edit?libraryId=1");
    fireEvent.change(await screen.findByLabelText("Max Items"), { target: { value: "100" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Collection" }));
    await closedTo("Admin collections page");
    expect(v2Recorder.writes()).toEqual(goldens.adminEditMDBList);
  });

  it("sends the whole TMDB chart source_config on a rename", async () => {
    v2Recorder.answer(
      "GET /api/v2/admin/collections/{id}",
      adminSyncedCollection("tmdb", {
        source_url: "tmdb://trending/movie/week",
        source_config: {
          mode: "tmdb_preset",
          preset: "trending",
          media_type: "movie",
          time_window: "week",
          limit: 40,
        },
      }),
    );
    show(<></>, "/admin/collections/c1/edit?libraryId=1");
    fireEvent.change(await screen.findByLabelText("Title"), {
      target: { value: "Trending This Week" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save Collection" }));
    await closedTo("Admin collections page");
    expect(v2Recorder.writes()).toEqual(goldens.adminEditTMDBChart);
  });

  it("sends the whole TMDB list source_config on a rename", async () => {
    v2Recorder.answer(
      "GET /api/v2/admin/collections/{id}",
      adminSyncedCollection("tmdb", {
        source_url: "https://www.themoviedb.org/list/310",
        source_config: { mode: "tmdb_list", url: "https://www.themoviedb.org/list/310" },
      }),
    );
    show(<></>, "/admin/collections/c1/edit?libraryId=1");
    fireEvent.change(await screen.findByLabelText("Title"), {
      target: { value: "Festival Picks" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save Collection" }));
    await closedTo("Admin collections page");
    expect(v2Recorder.writes()).toEqual(goldens.adminEditTMDBList);
  });

  it("keeps a Trakt source as stored and sends no source_url", async () => {
    v2Recorder.answer(
      "GET /api/v2/admin/collections/{id}",
      adminSyncedCollection("trakt", {
        source_url: "trakt://recommended/movie/p-owner",
        source_config: {
          mode: "trakt_preset",
          preset: "recommended",
          media_type: "movie",
          profile_id: "p-owner",
          limit: 40,
        },
      }),
    );
    show(<></>, "/admin/collections/c1/edit?libraryId=1");
    fireEvent.change(await screen.findByLabelText("Title"), { target: { value: "For you" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Collection" }));
    await closedTo("Admin collections page");
    expect(v2Recorder.writes()).toEqual(goldens.adminEditTrakt);
  });
});

describe("personal synced-list editor", () => {
  beforeEach(() => {
    v2Recorder.answer(
      "GET /api/v2/collections/{id}",
      personalSyncedCollection("mdblist", {
        source_url: "https://mdblist.com/lists/user/top-watched",
        source_config: { limit: 50, library_ids: [1] },
      }),
    );
  });

  it("sends only what changed", async () => {
    show(<></>, "/collections/c1/edit");
    fireEvent.change(await screen.findByLabelText("Name"), { target: { value: "Top Watched" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await vi.waitFor(() => expect(v2Recorder.writes()).toHaveLength(1));
    expect(v2Recorder.writes()).toEqual(goldens.personalSyncedRename);
  });

  it("clears Max items with max_items 0", async () => {
    show(<></>, "/collections/c1/edit");
    fireEvent.change(await screen.findByLabelText("Max items"), { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await vi.waitFor(() => expect(v2Recorder.writes()).toHaveLength(1));
    expect(v2Recorder.writes()).toEqual(goldens.personalSyncedClearLimit);
  });
});
