// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import WatchTonightEntry from "./WatchTonightEntry";

vi.mock("./WatchTonightDialog", () => ({
  default: ({ onOpenChange }: { onOpenChange: (open: boolean) => void }) => (
    <div role="dialog">
      <button onClick={() => onOpenChange(false)}>Close picks</button>
    </div>
  ),
}));
afterEach(cleanup);
it("opens the chooser on demand and closes it", async () => {
  render(<WatchTonightEntry />);
  expect(screen.queryByRole("dialog")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Help me choose" }));
  await screen.findByRole("dialog");
  fireEvent.click(screen.getByRole("button", { name: "Close picks" }));
  expect(screen.queryByRole("dialog")).toBeNull();
});
