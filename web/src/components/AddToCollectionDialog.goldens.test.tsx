/**
 * Goldens: what today's Add to collection dialog lists and which route an add
 * takes. An acting admin also gets every library's server manual collections
 * (the group a later change removes); a profile's own manual collections add
 * through the personal route.
 */
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import getLibraryCollectionsOk from "../../../contracts/api/v2/fixtures/get_library_collections_ok.json";
import { goldens } from "@/test/fixtures/collectionBodies";
import { installV2Recorder, v2Recorder } from "@/test/v2Recorder";
import AddToCollectionDialog from "./AddToCollectionDialog";

vi.mock("@/api/v2/request", async () => (await import("@/test/v2Recorder")).mockV2Request());
const account = vi.hoisted(() => ({ actingAdmin: false }));
vi.mock("@/hooks/useIsActingAdmin", () => ({ useIsActingAdmin: () => account.actingAdmin }));
vi.mock("@/hooks/useCurrentProfile", () => ({
  useCurrentProfile: () => ({ profile: { id: "p-owner" } }),
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
  account.actingAdmin = false;
  // The fixture's server collection shares its id with the personal fixture.
  v2Recorder.answer("GET /api/v2/library/{id}/collections", {
    ...getLibraryCollectionsOk,
    collections: getLibraryCollectionsOk.collections.map((entry) => ({ ...entry, id: "lc1" })),
  });
});

function show() {
  const onOpenChange = vi.fn();
  render(
    <QueryClientProvider
      client={
        new QueryClient({
          defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
        })
      }
    >
      <AddToCollectionDialog
        open
        onOpenChange={onOpenChange}
        mediaItemId="movie:heat-1995"
        itemTitle="Heat"
      />
    </QueryClientProvider>,
  );
  return onOpenChange;
}

/** Each group heading with the collection names under it, as the dialog lists them. */
function listedGroups() {
  return within(screen.getByRole("dialog"))
    .getAllByRole("listitem")
    .map((group) => {
      const [heading, ...rows] = Array.from(group.children);
      const label = (row: Element) =>
        Array.from(row.querySelectorAll("span"), (part) => part.textContent).join(" · ");
      return { group: heading!.textContent, collections: rows.map(label) };
    });
}

async function add(title: string, onOpenChange: ReturnType<typeof vi.fn>) {
  fireEvent.click(await screen.findByRole("button", { name: new RegExp(`^${title}`) }));
  fireEvent.click(screen.getByRole("button", { name: "Add" }));
  await vi.waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
}

describe("Add to collection", () => {
  it("lists only the profile's own manual collections for a regular profile", async () => {
    show();
    await screen.findByRole("button", { name: /^Rainy days/ });
    expect(listedGroups()).toEqual(goldens.addToCollectionGroups.profile);
    expect(v2Recorder.operations()).toEqual(goldens.addToCollectionReads.profile);
  });

  it("adds a title to a personal collection through the personal route", async () => {
    const onOpenChange = show();
    await add("Rainy days", onOpenChange);
    expect(v2Recorder.writes()).toEqual(goldens.addToPersonalCollection);
  });

  it("also lists every library's server manual collections for an acting admin", async () => {
    account.actingAdmin = true;
    show();
    await screen.findByRole("button", { name: /^Oscar Winners/ });
    expect(listedGroups()).toEqual(goldens.addToCollectionGroups.actingAdmin);
    expect(v2Recorder.operations()).toEqual(goldens.addToCollectionReads.actingAdmin);
  });

  it("adds a title to a server collection through the admin route", async () => {
    account.actingAdmin = true;
    const onOpenChange = show();
    await add("Oscar Winners", onOpenChange);
    expect(v2Recorder.writes()).toEqual(goldens.addToServerCollection);
  });
});
