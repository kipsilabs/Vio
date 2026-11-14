import { describe, expect, it } from "vitest";

import { planInventoryDescribesRequestedFile } from "./playbackInventoryMirror";

describe("planInventoryDescribesRequestedFile", () => {
  it("is true while the plan serves the requested row", () => {
    // A virtual rotation keeps effective_media_file_id at the requested row and
    // moves virtual_source_revision instead, so the inventory still describes
    // the viewer's selection.
    expect(
      planInventoryDescribesRequestedFile({
        requested_media_file_id: 7,
        effective_media_file_id: 7,
      }),
    ).toBe(true);
  });

  it("is false when the plan served a different row", () => {
    // A substitution (or a lower version chosen for this device) carries its own
    // stream table; the requested version's pre-play menu must not adopt it.
    expect(
      planInventoryDescribesRequestedFile({
        requested_media_file_id: 881207,
        effective_media_file_id: 1073913,
      }),
    ).toBe(false);
  });

  it("is false without a plan or ids", () => {
    expect(planInventoryDescribesRequestedFile(null)).toBe(false);
    expect(planInventoryDescribesRequestedFile(undefined)).toBe(false);
    expect(planInventoryDescribesRequestedFile({})).toBe(false);
    expect(
      planInventoryDescribesRequestedFile({
        requested_media_file_id: 7,
        effective_media_file_id: 0,
      }),
    ).toBe(false);
  });
});
