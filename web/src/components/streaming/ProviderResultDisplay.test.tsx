// @vitest-environment jsdom
import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ProviderResultDisplayToggle, ProviderResultText } from "./ProviderResultDisplay";
import { providerResultText, setProviderResultEnrichment } from "./useProviderResultDisplay";

afterEach(() => {
  vi.restoreAllMocks();
  act(() => setProviderResultEnrichment(false));
});

it("remembers enrichment across menus and reacts to other tabs", () => {
  localStorage.clear();
  const first = render(<ProviderResultDisplayToggle />);
  fireEvent.click(screen.getByRole("switch"));
  expect(screen.getByRole("switch")).toBeChecked();
  first.unmount();
  render(<ProviderResultDisplayToggle />);
  expect(screen.getByRole("switch")).toBeChecked();
  act(() => {
    localStorage.clear();
    window.dispatchEvent(new StorageEvent("storage"));
  });
  expect(screen.getByRole("switch")).not.toBeChecked();
});

it("allows a session choice when storage writes fail", () => {
  localStorage.clear();
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
    throw new Error("Quota exceeded");
  });
  render(<ProviderResultDisplayToggle />);
  fireEvent.click(screen.getByRole("switch"));
  expect(screen.getByRole("switch")).toBeChecked();
  fireEvent.click(screen.getByRole("switch"));
  expect(screen.getByRole("switch")).not.toBeChecked();
});

it("renders provider markup as plain text", () => {
  const text = '<img src=x onerror="alert(1)">\n🇪🇸 Spanish';
  const { container } = render(<ProviderResultText text={text} />);
  expect(container.textContent).toBe(text);
  expect(container.querySelector("img")).toBeNull();
});

it("uses provider text only for virtual candidates with meaningful display labels", () => {
  expect(providerResultText({ container: "mkv", edition_raw: "Director’s cut" })).toBeUndefined();
  expect(
    providerResultText({ container: "virtual", edition_raw: "virtual://movie/tt1?result=a" }),
  ).toBeUndefined();
  expect(
    providerResultText({
      file_path: "virtual://movie/tt1?result=a",
      edition_raw: "⚡ Name\nDescription",
    }),
  ).toBe("⚡ Name\nDescription");
});
