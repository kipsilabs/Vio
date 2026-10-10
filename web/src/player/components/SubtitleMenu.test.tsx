// @vitest-environment jsdom

import { fireEvent, render, screen } from "@testing-library/react";
import { createElement } from "react";
import { describe, expect, it, vi } from "vitest";

import type { PlayerSubtitleInfo } from "../types";
import { SubtitleMenu } from "./SubtitleMenu";

// The appearance panel reads settings through react-query; the menu's own
// rendering does not need a provider.
vi.mock("./SubtitleAppearancePanel", () => ({
  SubtitleAppearancePanel: () => null,
}));

function subtitleTrack(overrides: Partial<PlayerSubtitleInfo> = {}): PlayerSubtitleInfo {
  return {
    index: 0,
    language: "en",
    codec: "pgs",
    label: "English",
    source: "embedded",
    url: "/stream/session-1/subtitles/0.sup",
    ...overrides,
  };
}

function renderMenu(
  tracks: PlayerSubtitleInfo[],
  onSelect = vi.fn(),
  activeIndex: number | null = null,
) {
  const view = render(
    createElement(SubtitleMenu, {
      tracks,
      activeIndex,
      onSelect,
      delayMs: 0,
      onDelayChange: () => {},
    }),
  );
  fireEvent.click(screen.getByRole("button", { name: /(Enable|Disable) captions/ }));
  return { view, onSelect };
}

describe("SubtitleMenu", () => {
  it("collapses identical probed streams to one row and keeps the first ordinal", () => {
    const { onSelect } = renderMenu([
      subtitleTrack({ index: 13 }),
      subtitleTrack({ index: 14 }),
      subtitleTrack({ index: 23 }),
    ]);

    const rows = screen.getAllByRole("menuitem");
    // Off + one deduped track + Appearance.
    expect(rows).toHaveLength(3);
    const trackRow = screen.getByRole("menuitem", { name: /English/ });
    expect(trackRow).toBeTruthy();

    fireEvent.click(trackRow);
    expect(onSelect).toHaveBeenCalledWith(13);
  });

  it("keeps tracks that differ in forced or hearing-impaired flags", () => {
    renderMenu([
      subtitleTrack({ index: 0 }),
      subtitleTrack({ index: 1, forced: true }),
      subtitleTrack({ index: 2, hearing_impaired: true }),
    ]);

    // Off + three distinct tracks + Appearance.
    expect(screen.getAllByRole("menuitem")).toHaveLength(5);
  });

  it("keeps identically labelled tracks with distinct track ids and selects the second", () => {
    const { onSelect } = renderMenu([
      subtitleTrack({ index: 13, track_id: "file:7:subtitle:13" }),
      subtitleTrack({ index: 14, track_id: "file:7:subtitle:14" }),
    ]);

    // Off + two distinct tracks + Appearance.
    expect(screen.getAllByRole("menuitem")).toHaveLength(4);
    const trackRows = screen.getAllByRole("menuitem", { name: /English/ });
    expect(trackRows).toHaveLength(2);

    fireEvent.click(trackRows[1] as HTMLElement);
    expect(onSelect).toHaveBeenCalledWith(14);
  });

  it("marks the second identically labelled track active when the server selected it", () => {
    renderMenu(
      [
        subtitleTrack({ index: 13, track_id: "file:7:subtitle:13" }),
        subtitleTrack({ index: 14, track_id: "file:7:subtitle:14" }),
      ],
      vi.fn(),
      14,
    );

    const trackRows = screen.getAllByRole("menuitem", { name: /English/ });
    expect(trackRows).toHaveLength(2);
    expect(trackRows[0]?.textContent).not.toContain("✓");
    expect(trackRows[1]?.textContent).toContain("✓");
  });

  it("locks the menu shut while the session inventory is switching", () => {
    const onSelect = vi.fn();
    const props = {
      tracks: [subtitleTrack({ index: 3 })],
      activeIndex: null,
      onSelect,
      delayMs: 0,
      onDelayChange: () => {},
    };
    const view = render(createElement(SubtitleMenu, { ...props, locked: true }));

    const trigger = screen.getByRole("button", { name: "Enable captions" });
    expect(trigger).toBeDisabled();
    expect(trigger).toHaveAttribute("aria-disabled", "true");
    fireEvent.click(trigger);
    expect(screen.queryByRole("menu")).toBeNull();
    expect(onSelect).not.toHaveBeenCalled();

    // A switch beginning mid-menu closes it rather than leave the outgoing
    // inventory selectable.
    view.rerender(createElement(SubtitleMenu, { ...props, locked: false }));
    fireEvent.click(trigger);
    expect(screen.getByRole("menu")).toBeInTheDocument();
    view.rerender(createElement(SubtitleMenu, { ...props, locked: true }));
    expect(screen.queryByRole("menu")).toBeNull();
    expect(onSelect).not.toHaveBeenCalled();
  });

  it("marks the menu unverified while the inventory is provisional", () => {
    render(
      createElement(SubtitleMenu, {
        tracks: [subtitleTrack()],
        activeIndex: null,
        onSelect: () => {},
        delayMs: 0,
        onDelayChange: () => {},
        provisional: true,
      }),
    );
    fireEvent.click(screen.getByRole("button", { name: /(Enable|Disable) captions/ }));

    expect(screen.getByText("Unverified")).toBeTruthy();
  });

  it("hides the unverified marker once the inventory is verified", () => {
    renderMenu([subtitleTrack()]);

    expect(screen.queryByText("Unverified")).toBeNull();
  });

  it("shows discovery state when subtitle tracks are empty but inventory is provisional", () => {
    render(
      createElement(SubtitleMenu, {
        tracks: [],
        activeIndex: null,
        onSelect: () => {},
        delayMs: 0,
        onDelayChange: () => {},
        mediaFileId: 42,
        provisional: true,
      }),
    );
    fireEvent.click(screen.getByRole("button", { name: /(Enable|Disable) captions/ }));

    expect(screen.getByText("Discovering subtitles…")).toBeTruthy();
    expect(screen.getByText("Unverified")).toBeTruthy();
  });

  it("shows explicit empty state when verified with no subtitles", () => {
    render(
      createElement(SubtitleMenu, {
        tracks: [],
        activeIndex: null,
        onSelect: () => {},
        delayMs: 0,
        onDelayChange: () => {},
        mediaFileId: 42,
        provisional: false,
      }),
    );
    fireEvent.click(screen.getByRole("button", { name: /(Enable|Disable) captions/ }));

    expect(screen.getByText("No subtitles available")).toBeTruthy();
    expect(screen.queryByText("Discovering subtitles…")).toBeNull();
  });

  it("shows failure state when discovery failed and subtitle tracks are empty", () => {
    render(
      createElement(SubtitleMenu, {
        tracks: [],
        activeIndex: null,
        onSelect: () => {},
        delayMs: 0,
        onDelayChange: () => {},
        mediaFileId: 42,
        provisional: true,
        failed: true,
      }),
    );
    fireEvent.click(screen.getByRole("button", { name: /(Enable|Disable) captions/ }));

    expect(screen.getByText("Subtitle discovery failed")).toBeTruthy();
    expect(screen.queryByText("Discovering subtitles…")).toBeNull();
  });

  it("shows discovery banner even when declared tracks are present while provisional", () => {
    render(
      createElement(SubtitleMenu, {
        tracks: [subtitleTrack()],
        activeIndex: null,
        onSelect: () => {},
        delayMs: 0,
        onDelayChange: () => {},
        mediaFileId: 42,
        provisional: true,
      }),
    );
    fireEvent.click(screen.getByRole("button", { name: /(Enable|Disable) captions/ }));

    expect(screen.getByText("Discovering subtitles…")).toBeTruthy();
    expect(screen.getByText("English")).toBeTruthy();
  });

  it("shows failure banner even when declared tracks are present", () => {
    render(
      createElement(SubtitleMenu, {
        tracks: [subtitleTrack()],
        activeIndex: null,
        onSelect: () => {},
        delayMs: 0,
        onDelayChange: () => {},
        mediaFileId: 42,
        provisional: true,
        failed: true,
      }),
    );
    fireEvent.click(screen.getByRole("button", { name: /(Enable|Disable) captions/ }));

    expect(screen.getByText("Subtitle discovery failed")).toBeTruthy();
    expect(screen.getByText("English")).toBeTruthy();
  });
});
