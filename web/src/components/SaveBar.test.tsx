import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { SaveBar } from "./SaveBar";

function renderBar(props: Partial<Parameters<typeof SaveBar>[0]> = {}) {
  return render(
    <SaveBar dirtyCount={2} onSave={vi.fn()} onDiscard={vi.fn()} isSaving={false} {...props} />,
  );
}

describe("SaveBar", () => {
  it("stays hidden while the tab is clean", () => {
    const { container } = renderBar({ dirtyCount: 0 });

    expect(container).toBeEmptyDOMElement();
  });

  it("counts the staged changes and offers both actions", async () => {
    const onSave = vi.fn();
    const onDiscard = vi.fn();
    renderBar({ dirtyCount: 3, onSave, onDiscard });

    expect(screen.getByText("3 unsaved changes")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Discard" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(onDiscard).toHaveBeenCalledTimes(1);
    expect(onSave).toHaveBeenCalledTimes(1);
  });

  it("does not pass the click event to a save callback that accepts selected keys", async () => {
    const onSave = vi.fn((selectedKeys?: string[]) =>
      selectedKeys?.includes("artwork.storage_backend"),
    );
    renderBar({ onSave });

    await userEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(onSave.mock.calls).toEqual([[]]);
  });

  it("uses the singular form for one change", () => {
    renderBar({ dirtyCount: 1 });

    expect(screen.getByText("1 unsaved change")).toBeInTheDocument();
  });

  it("says nothing about restarts", () => {
    renderBar({ dirtyCount: 4 });

    expect(screen.queryByText(/restart/i)).not.toBeInTheDocument();
  });

  it("disables saving while a save is in flight", () => {
    renderBar({ isSaving: true });

    expect(screen.getByRole("button", { name: "Saving..." })).toBeDisabled();
  });

  // The restart prompt belongs to the admin shell (see
  // components/admin/RestartBanner.test.tsx); the pill must never grow one.
  it("renders no restart prompt of its own", () => {
    renderBar({ dirtyCount: 2 });

    expect(screen.queryByText("Restart required")).not.toBeInTheDocument();
  });

  it("keeps one status wrapper around the whole settings pill", () => {
    renderBar();

    expect(screen.queryByRole("region")).not.toBeInTheDocument();
    expect(screen.getByRole("status")).toContainElement(
      screen.getByRole("button", { name: "Save" }),
    );
  });

  it("names the staged fields in place of the count", () => {
    renderBar({ message: "Name and description not saved" });

    expect(screen.getByText("Name and description not saved")).toBeInTheDocument();
    expect(screen.queryByText("2 unsaved changes")).not.toBeInTheDocument();
  });

  it("relabels both actions and holds the primary one back until it can run", async () => {
    const onDiscard = vi.fn();
    renderBar({
      saveLabel: "Create collection",
      discardLabel: "Cancel",
      canSave: false,
      onDiscard,
    });

    expect(screen.getByRole("button", { name: "Create collection" })).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onDiscard).toHaveBeenCalledTimes(1);
  });
});

describe("SaveBar on a page", () => {
  it("is a labelled region that announces only its message", () => {
    renderBar({ placement: "page", message: "Name not saved" });

    const region = screen.getByRole("region", { name: "Unsaved changes" });
    const status = within(region).getByRole("status");
    expect(status).toHaveTextContent("Name not saved");
    expect(within(status).queryByRole("button")).not.toBeInTheDocument();
    expect(within(region).getByRole("button", { name: "Save" })).toBeEnabled();
    expect(within(region).getByRole("button", { name: "Discard" })).toBeEnabled();
  });

  // The page bar starts at the sidebar edge either shell publishes in
  // `--app-sidebar-offset` (260px, the 64px rail, or the admin 240px) instead of
  // assuming the admin sidebar, and rises above the background playback bar by
  // the `--main-inset-bottom` both shells set on <main> while that bar shows.
  it("spans the main column and clears the background playback bar", () => {
    renderBar({ placement: "page" });

    const dock = screen.getByRole("region", { name: "Unsaved changes" }).parentElement;
    expect(dock).toHaveClass(
      "fixed",
      "left-[var(--app-sidebar-offset,0px)]",
      "right-0",
      "bottom-[calc(var(--main-inset-bottom,0px)+0.75rem)]",
    );
    expect(dock).not.toHaveClass("lg:left-[240px]");
  });

  it("stays hidden while nothing is staged", () => {
    const { container } = renderBar({ placement: "page", dirtyCount: 0 });

    expect(container).toBeEmptyDOMElement();
  });
});
