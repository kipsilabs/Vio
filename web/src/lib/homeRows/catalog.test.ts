import { describe, expect, it } from "vitest";
import { ROW_GROUP_LABELS, rowKindGroup, rowKindLabel } from "./catalog";

describe("row kind catalog", () => {
  it("gives each stored row type a plain name and never a raw type key", () => {
    expect(rowKindLabel("next_up")).toBe("On deck");
    expect(rowKindLabel("custom_filter")).toBe("Titles matching rules");
    expect(rowKindLabel("award_winners")).toBe("Award winners (no longer offered)");
    expect(rowKindLabel("some_future_type")).toBe("Row");
  });

  it("files each row type under a named picker group", () => {
    expect(rowKindGroup("continue_watching")).toBe("keep");
    expect(rowKindGroup("trending_discover")).toBe("popular");
    expect(rowKindGroup("some_future_type")).toBe("collections");
    expect(ROW_GROUP_LABELS[rowKindGroup("hidden_gems")]).toBe("Moods & themes");
  });
});
