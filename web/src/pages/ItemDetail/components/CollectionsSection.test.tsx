import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";

import { UICustomizationContext } from "@/contexts/uiCustomizationContext";
import type { ItemCollection } from "@/api/types";
import CollectionsSection from "./CollectionsSection";

const COLLECTION: ItemCollection = {
  id: "oscar-winners",
  title: "Oscar Winners",
  poster_url: "/collections/oscar-winners.jpg",
  poster_thumbhash: "abc123",
  item_count: 12,
};

function render(collections?: ItemCollection[], posterSize: "compact" | "standard" = "standard") {
  return renderToStaticMarkup(
    <MemoryRouter>
      <UICustomizationContext.Provider
        value={{
          cardPresentation: { poster_size: posterSize, caption: "title" },
          cardPresentationSource: "profile_client",
          primaryMenu: null,
          primaryMenuSource: "default",
          shortcuts: { items: [] },
          isSupported: true,
          supportsAtomicShortcuts: true,
          isLoading: false,
          isUnavailable: false,
        }}
      >
        <CollectionsSection collections={collections} />
      </UICustomizationContext.Provider>
    </MemoryRouter>,
  );
}

describe("CollectionsSection", () => {
  it("renders a chip per collection with its poster, title, and item count", () => {
    const markup = render([COLLECTION]);

    expect(markup).toContain('src="/collections/oscar-winners.jpg"');
    expect(markup).toContain("Oscar Winners");
    expect(markup).toContain("12 items");
    expect(markup).toContain("embla__viewport");
  });

  it("pluralizes the item count for a single-item collection", () => {
    const markup = render([{ ...COLLECTION, item_count: 1 }]);

    expect(markup).toContain("1 item");
    expect(markup).not.toContain("1 items");
  });

  it("links each chip to the collection's browse view", () => {
    const markup = render([
      COLLECTION,
      { ...COLLECTION, id: "new-releases", title: "New Releases" },
    ]);

    expect(markup).toContain(
      'href="/catalog?source=library_collection&amp;collection_id=oscar-winners&amp;title=Oscar+Winners"',
    );
    expect(markup).toContain(
      'href="/catalog?source=library_collection&amp;collection_id=new-releases&amp;title=New+Releases"',
    );
  });

  it("renders nothing when the item is in no collections", () => {
    expect(render([])).toBe("");
    expect(render(undefined)).toBe("");
  });

  it("sizes the chips with the viewer's poster density", () => {
    expect(render([COLLECTION], "compact")).toContain(
      "w-[120px] shrink-0 sm:w-[140px] lg:w-[160px]",
    );
    expect(render([COLLECTION], "standard")).toContain(
      "w-[140px] shrink-0 sm:w-[160px] lg:w-[185px]",
    );
  });
});
