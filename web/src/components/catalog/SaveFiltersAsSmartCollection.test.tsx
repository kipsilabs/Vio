import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";

import { parseCatalogSearchParams } from "@/pages/catalogSearchParams";

import SaveFiltersAsSmartCollection from "./SaveFiltersAsSmartCollection";

const OR_FILTERS =
  "source=query&library_id=3&match=any" +
  "&groups%5B0%5D%5Bmatch%5D=all&groups%5B0%5D%5Brules%5D%5B0%5D%5Bfield%5D=genre" +
  "&groups%5B0%5D%5Brules%5D%5B0%5D%5Bop%5D=contains&groups%5B0%5D%5Brules%5D%5B0%5D%5Bvalue%5D=Action" +
  "&groups%5B1%5D%5Bmatch%5D=all&groups%5B1%5D%5Brules%5D%5B0%5D%5Bfield%5D=year" +
  "&groups%5B1%5D%5Brules%5D%5B0%5D%5Bop%5D=gte&groups%5B1%5D%5Brules%5D%5B0%5D%5Bvalue%5D=2000";

function renderAction(search: string) {
  const state = parseCatalogSearchParams(new URLSearchParams(search));
  render(
    <MemoryRouter>
      <SaveFiltersAsSmartCollection state={state} />
    </MemoryRouter>,
  );
  return screen.getByRole("link", { name: /Save filters as smart collection/ });
}

describe("SaveFiltersAsSmartCollection", () => {
  it("links to the seeded wizard with the structured filters, including top-level OR", () => {
    const link = renderAction(OR_FILTERS);
    const href = new URL(link.getAttribute("href")!, "http://example.test");
    expect(href.pathname).toBe("/collections/new");
    expect(href.searchParams.get("smart")).toBe("1");
    expect(href.searchParams.get("match")).toBe("any");
    expect(href.searchParams.get("library_id")).toBe("3");
    // No text search, so no exclusion notice.
    expect(screen.queryByText(/Text search isn/)).not.toBeInTheDocument();
  });

  it("keeps the link usable during text search, with a notice that the search is excluded", () => {
    const link = renderAction(`q=cruise&${OR_FILTERS}`);
    expect(link).toHaveAttribute("aria-describedby");
    const notice = document.getElementById(link.getAttribute("aria-describedby")!);
    expect(notice).toHaveTextContent("Text search isn’t included — only the filters are saved.");

    const href = new URL(link.getAttribute("href")!, "http://example.test");
    expect(href.searchParams.has("q")).toBe(false);
    expect(href.searchParams.get("match")).toBe("any");
    expect(href.searchParams.get("library_id")).toBe("3");
  });
});
