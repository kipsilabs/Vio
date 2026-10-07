import { describe, expect, it } from "vitest";

import itemFixture from "../../../../contracts/api/v2/fixtures/get_catalog_item_ok.json";
import { catalogItemDetailFromV2 } from "@/api/v2/catalog";

// catalogItemDetailFromV2 builds ItemDetail field by field, so a contract
// member the mapper forgets to copy disappears silently: types compile, the
// API returns the row, and the detail page renders nothing. The Collections
// row is pinned here for the same reason the advisory badge is.
describe("catalogItemDetailFromV2 collections", () => {
  it("carries the item's collections through the mapper", () => {
    const detail = catalogItemDetailFromV2(
      itemFixture as Parameters<typeof catalogItemDetailFromV2>[0],
    );

    expect(detail.collections).toEqual([
      {
        id: "oscar-winners",
        title: "Oscar Winners",
        poster_url: "https://cdn.example.invalid/oscar-winners.jpg",
        item_count: 12,
      },
    ]);
  });

  it("keeps an empty memberships list empty rather than dropping it", () => {
    const detail = catalogItemDetailFromV2({
      ...(itemFixture as Parameters<typeof catalogItemDetailFromV2>[0]),
      collections: [],
    });

    expect(detail.collections).toEqual([]);
  });
});
