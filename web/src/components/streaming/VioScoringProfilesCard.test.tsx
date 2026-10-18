// @vitest-environment jsdom

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { SortCriteriaSummary } from "./SortCriteriaSummary";
import { VioScoringProfilesCard } from "./VioScoringProfilesCard";
import type { QualityProfileRule } from "./scoringPresets";

class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
if (typeof globalThis.ResizeObserver === "undefined") {
  (globalThis as unknown as { ResizeObserver: typeof ResizeObserverStub }).ResizeObserver =
    ResizeObserverStub;
}

if (!window.HTMLElement.prototype.hasPointerCapture) {
  window.HTMLElement.prototype.hasPointerCapture = () => false;
  window.HTMLElement.prototype.scrollIntoView = () => {};
}

function makeForm(values: Record<string, string>) {
  return {
    getValue: (key: string) => values[key],
    setValue: (key: string, value: string) => {
      values[key] = value;
    },
  };
}

describe("SortCriteriaSummary", () => {
  it("renders one chip per criterion in order", () => {
    render(
      <SortCriteriaSummary
        criteria={[
          { attribute: "size", direction: "desc" },
          { attribute: "bitrate", direction: "desc" },
        ]}
      />,
    );

    expect(screen.getByText("size ↓")).toBeInTheDocument();
    expect(screen.getByText("bitrate ↓")).toBeInTheDocument();
  });

  it("shows the default-order hint when the profile has no criteria", () => {
    const { rerender } = render(<SortCriteriaSummary criteria={[]} />);
    expect(screen.getByText("Default order")).toBeInTheDocument();

    rerender(<SortCriteriaSummary criteria={undefined} />);
    expect(screen.getByText("Default order")).toBeInTheDocument();
  });
});

describe("VioScoringProfilesCard sort summary", () => {
  it("shows chips for a configured profile and the hint for a default one", async () => {
    const user = userEvent.setup();
    const profiles: QualityProfileRule[] = [
      {
        label: "Big first",
        preferred_order: 1,
        sort: [
          { attribute: "size", direction: "desc" },
          { attribute: "bitrate", direction: "asc" },
        ],
      },
      { label: "Defaulted", preferred_order: 2 },
    ];
    const form = makeForm({
      "virtual_library.quality_preset": "custom",
      "virtual_library.quality_profiles": JSON.stringify(profiles),
      "virtual_library.enable_quality_profiles": "true",
    });

    render(<VioScoringProfilesCard form={form} defaultExpanded />);
    await user.click(screen.getByRole("tab", { name: "Quality Profiles & Resolution" }));

    expect(screen.getByText("size ↓")).toBeInTheDocument();
    expect(screen.getByText("bitrate ↑")).toBeInTheDocument();
    expect(screen.getByText("Default order")).toBeInTheDocument();
  });
});

describe("VioScoringProfilesCard server order", () => {
  it("lets admins trust the provider order without replacing custom weights", async () => {
    const user = userEvent.setup();
    const values = {
      "virtual_library.custom_format_preset": "custom",
      "virtual_library.custom_formats":
        '[{"id":"saved","name":"Saved rule","category":"custom","pattern":"saved","patternType":"token","enabled":true,"score":50}]',
    };
    const form = makeForm(values);
    const { rerender } = render(<VioScoringProfilesCard form={form} defaultExpanded />);
    await user.click(screen.getByRole("combobox", { name: "Custom format preset" }));
    await user.click(screen.getByRole("option", { name: "Trust server order" }));
    expect(values["virtual_library.custom_format_preset"]).toBe("server-order");
    expect(values["virtual_library.custom_formats"]).toContain("saved");
    rerender(<VioScoringProfilesCard form={form} defaultExpanded />);
    expect(screen.getByText(/Keep streams in the order returned/)).toBeInTheDocument();
    expect(screen.queryByText("Saved rule")).not.toBeInTheDocument();
  });
});
