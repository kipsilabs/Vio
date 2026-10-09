import { fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { SourceTypeSelector, type CollectionSourcePick } from "./adminCollectionsShared";

const mocks = vi.hoisted(() => ({ imports: true }));

vi.mock("@/hooks/queries/admin/collections", () => ({
  useAdminCollectionCapabilities: () => ({ data: { imports: mocks.imports } }),
}));

function renderSelector() {
  const onSelect = vi.fn<(type: CollectionSourcePick) => void>();
  render(<SourceTypeSelector onSelect={onSelect} />);
  return onSelect;
}

describe("admin SourceTypeSelector", () => {
  beforeEach(() => {
    mocks.imports = true;
  });

  it("renders the Smart tile alongside the other collection types", () => {
    renderSelector();
    expect(screen.getByRole("button", { name: /Manual Curate items by hand/ })).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /Smart Match titles with filters that stay up to date/ }),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /MDBList/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /TMDB/ })).toBeInTheDocument();
  });

  it("selects the smart type when the Smart tile is clicked", () => {
    const onSelect = renderSelector();
    fireEvent.click(
      screen.getByRole("button", { name: /Smart Match titles with filters that stay up to date/ }),
    );
    expect(onSelect).toHaveBeenCalledWith("smart");
  });

  it("keeps the Smart tile visible when import sources are unavailable", () => {
    mocks.imports = false;
    renderSelector();
    expect(
      screen.getByRole("button", { name: /Smart Match titles with filters that stay up to date/ }),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Manual / })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /MDBList/ })).not.toBeInTheDocument();
  });
});
