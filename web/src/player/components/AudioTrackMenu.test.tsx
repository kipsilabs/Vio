// @vitest-environment jsdom

import { fireEvent, render, screen } from "@testing-library/react";
import { createElement } from "react";
import { describe, expect, it, vi } from "vitest";
import { AudioTrackMenu } from "./AudioTrackMenu";

function renderMenu(tracks: Parameters<typeof AudioTrackMenu>[0]["tracks"]) {
  return render(
    createElement(AudioTrackMenu, {
      tracks,
      activeIndex: 0,
      onSelect: () => {},
      currentPosition: 0,
      open: true,
      onOpenChange: () => {},
      hideTrigger: true,
    }),
  );
}

describe("AudioTrackMenu", () => {
  it("joins the languages array into a multi-language descriptor", () => {
    renderMenu([
      {
        title: "English / French / Spanish",
        languages: ["en", "fr", "es"],
        codec: "eac3",
        layout: "5.1",
      },
    ]);
    expect(screen.getByText("English/French/Spanish · 5.1")).toBeTruthy();
  });

  it("falls back to the single language tag when languages is absent", () => {
    renderMenu([{ title: "French DTS", language: "fra", codec: "dts" }]);
    expect(screen.getByText("French")).toBeTruthy();
  });

  it("omits the language segment when no language resolves", () => {
    renderMenu([{ title: "Commentary", codec: "ac3", layout: "2.0" }]);
    expect(screen.getByText("2.0")).toBeTruthy();
  });

  it("keeps the trigger enabled with a single track and opens to that entry", () => {
    render(
      createElement(AudioTrackMenu, {
        tracks: [{ title: "English", codec: "eac3", channels: 6, default: true }],
        activeIndex: 0,
        onSelect: () => {},
        currentPosition: 0,
      }),
    );

    const trigger = screen.getByRole("button", { name: "Audio tracks" });
    expect(trigger).not.toHaveAttribute("aria-disabled");
    expect(trigger).not.toHaveClass("cursor-default");
    expect(trigger).not.toHaveClass("opacity-40");
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("menu")).toBeNull();

    fireEvent.click(trigger);

    expect(trigger).toHaveAttribute("aria-expanded", "true");
    const entry = screen.getByRole("menuitem");
    expect(entry).toHaveClass("text-blue-400");
    expect(entry).toHaveTextContent("English");
    expect(entry).toHaveTextContent("EAC3");
    expect(entry).toHaveTextContent("5.1");
    expect(entry).toHaveTextContent("Default");
    expect(entry).toHaveTextContent("\u2713");
  });

  it("locks the menu shut while the session inventory is switching", () => {
    const onSelect = vi.fn();
    const tracks = [
      { title: "English", codec: "eac3", channels: 6, default: true },
      { title: "French", codec: "aac", channels: 2 },
    ];
    render(
      createElement(AudioTrackMenu, {
        tracks,
        activeIndex: 0,
        onSelect,
        currentPosition: 0,
        locked: true,
      }),
    );

    const trigger = screen.getByRole("button", { name: "Audio tracks" });
    expect(trigger).toBeDisabled();
    expect(trigger).toHaveAttribute("aria-disabled", "true");
    fireEvent.click(trigger);
    expect(screen.queryByRole("menu")).toBeNull();
    expect(onSelect).not.toHaveBeenCalled();
  });

  it("closes an already-open menu when the inventory is locked", () => {
    const onSelect = vi.fn();
    const tracks = [
      { title: "English", codec: "eac3", channels: 6, default: true },
      { title: "French", codec: "aac", channels: 2 },
    ];
    const props = {
      tracks,
      activeIndex: 0,
      onSelect,
      currentPosition: 0,
    };
    const view = render(createElement(AudioTrackMenu, { ...props, open: true, hideTrigger: true }));
    expect(screen.getAllByRole("menuitem")).toHaveLength(2);

    view.rerender(
      createElement(AudioTrackMenu, { ...props, open: true, hideTrigger: true, locked: true }),
    );
    expect(screen.queryByRole("menu")).toBeNull();
    expect(onSelect).not.toHaveBeenCalled();
  });

  it("renders nothing when there are no tracks", () => {
    const { container } = render(
      createElement(AudioTrackMenu, {
        tracks: [],
        activeIndex: -1,
        onSelect: () => {},
        currentPosition: 0,
      }),
    );
    expect(container.firstChild).toBeNull();
    expect(screen.queryByRole("button", { name: "Audio tracks" })).toBeNull();
  });

  it("keeps multiple-track selection behavior unchanged", () => {
    const onSelect = vi.fn();
    render(
      createElement(AudioTrackMenu, {
        tracks: [
          { title: "English", codec: "eac3", channels: 6, default: true },
          { title: "French", codec: "aac", channels: 2 },
        ],
        activeIndex: 0,
        onSelect,
        currentPosition: 0,
      }),
    );

    fireEvent.click(screen.getByRole("button", { name: "Audio tracks" }));
    const entries = screen.getAllByRole("menuitem");
    expect(entries).toHaveLength(2);
    const [firstEntry, secondEntry] = entries;
    if (!firstEntry || !secondEntry) {
      throw new Error("expected two menu entries");
    }
    expect(firstEntry).toHaveClass("text-blue-400");
    expect(secondEntry).not.toHaveClass("text-blue-400");

    fireEvent.click(secondEntry);
    expect(onSelect).toHaveBeenCalledWith(1, 0);
  });

  it("collapses identical descriptors and selects the first entry's original slot", () => {
    const onSelect = vi.fn();
    const tracks = [
      ...Array.from({ length: 7 }, (_, i) => ({
        title: `Track ${i}`,
        codec: "aac",
        channels: 2,
        language: `l${i}`,
        index: i,
      })),
      // Same descriptor at two container indexes (7 and 9), as a probed
      // multi-language release can carry. The first wins.
      { title: "English AC3", codec: "ac3", channels: 6, language: "en", index: 7 },
      { title: "Commentary", codec: "ac3", channels: 2, language: "en", index: 8 },
      { title: "English AC3", codec: "ac3", channels: 6, language: "en", index: 9 },
    ];

    render(
      createElement(AudioTrackMenu, {
        tracks,
        activeIndex: 7,
        onSelect,
        currentPosition: 0,
        open: true,
        onOpenChange: () => {},
        hideTrigger: true,
      }),
    );

    const entries = screen.getAllByRole("menuitem");
    // One duplicate collapsed: 10 tracks become 9 rows.
    expect(entries).toHaveLength(9);
    // The retained English entry keeps inventory slot 7, and its active state
    // still matches the plan's selection.
    const retained = entries[7]!;
    expect(retained).toHaveTextContent("English AC3");
    expect(retained).toHaveClass("text-blue-400");

    fireEvent.click(retained);
    expect(onSelect).toHaveBeenCalledWith(7, 0);
  });

  it("does not collapse tracks that differ in language", () => {
    renderMenu([
      { title: "English", codec: "ac3", channels: 6, language: "en" },
      { title: "English", codec: "ac3", channels: 6, language: "es" },
    ]);

    expect(screen.getAllByRole("menuitem")).toHaveLength(2);
  });

  it("marks the menu unverified while the inventory is provisional", () => {
    render(
      createElement(AudioTrackMenu, {
        tracks: [{ title: "English", codec: "eac3", channels: 6 }],
        activeIndex: 0,
        onSelect: () => {},
        currentPosition: 0,
        open: true,
        onOpenChange: () => {},
        hideTrigger: true,
        provisional: true,
      }),
    );

    expect(screen.getByText("Unverified")).toBeTruthy();
  });

  it("hides the unverified marker once the inventory is verified", () => {
    renderMenu([{ title: "English", codec: "eac3", channels: 6 }]);

    expect(screen.queryByText("Unverified")).toBeNull();
  });

  it("shows discovery state when tracks are empty but inventory is provisional", () => {
    render(
      createElement(AudioTrackMenu, {
        tracks: [],
        activeIndex: 0,
        onSelect: () => {},
        currentPosition: 0,
        open: true,
        onOpenChange: () => {},
        hideTrigger: false,
        provisional: true,
      }),
    );

    expect(screen.getByText("Discovering audio tracks…")).toBeTruthy();
    expect(screen.getByText("Unverified")).toBeTruthy();
  });

  it("shows failure state when discovery failed and tracks are empty", () => {
    render(
      createElement(AudioTrackMenu, {
        tracks: [],
        activeIndex: 0,
        onSelect: () => {},
        currentPosition: 0,
        open: true,
        onOpenChange: () => {},
        hideTrigger: false,
        provisional: true,
        failed: true,
      }),
    );

    expect(screen.getByText("Track discovery failed")).toBeTruthy();
    expect(screen.queryByText("Discovering audio tracks…")).toBeNull();
  });

  it("shows discovery banner even when declared tracks are present while provisional", () => {
    render(
      createElement(AudioTrackMenu, {
        tracks: [{ title: "English", codec: "aac", channels: 2 }],
        activeIndex: 0,
        onSelect: () => {},
        currentPosition: 0,
        open: true,
        onOpenChange: () => {},
        hideTrigger: false,
        provisional: true,
      }),
    );

    expect(screen.getByText("Discovering audio tracks…")).toBeTruthy();
    expect(screen.getByText("English")).toBeTruthy();
  });

  it("shows failure banner even when declared tracks are present", () => {
    render(
      createElement(AudioTrackMenu, {
        tracks: [{ title: "English", codec: "aac", channels: 2 }],
        activeIndex: 0,
        onSelect: () => {},
        currentPosition: 0,
        open: true,
        onOpenChange: () => {},
        hideTrigger: false,
        provisional: true,
        failed: true,
      }),
    );

    expect(screen.getByText("Track discovery failed")).toBeTruthy();
    expect(screen.getByText("English")).toBeTruthy();
  });

  it("renders explicit empty state when tracks are empty and open while verified", () => {
    render(
      createElement(AudioTrackMenu, {
        tracks: [],
        activeIndex: 0,
        onSelect: () => {},
        currentPosition: 0,
        open: true,
        onOpenChange: () => {},
        provisional: false,
        failed: false,
      }),
    );

    expect(screen.getByText("No audio tracks available")).toBeTruthy();
  });

  it("returns null when tracks are empty and closed while verified", () => {
    const { container } = render(
      createElement(AudioTrackMenu, {
        tracks: [],
        activeIndex: 0,
        onSelect: () => {},
        currentPosition: 0,
        open: false,
        onOpenChange: () => {},
        provisional: false,
        failed: false,
      }),
    );

    expect(container.firstChild).toBeNull();
  });
});
