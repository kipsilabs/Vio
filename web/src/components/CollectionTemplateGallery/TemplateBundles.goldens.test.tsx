/**
 * Goldens: the bodies today's template bundle view sends for a dry run and
 * for the apply job, each with its default hero sections, with every hero
 * turned off, and with Delete Existing on.
 */
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

import getAdminTemplateJobCompleted from "../../../../contracts/api/v2/fixtures/get_admin_collection_template_job_completed.json";
import type { Library } from "@/api/types";
import { adminCapabilities } from "@/test/fixtures/collectionAnswers";
import { goldens } from "@/test/fixtures/collectionBodies";
import { installV2Recorder, v2Recorder } from "@/test/v2Recorder";
import { CollectionTemplateGallery } from "./CollectionTemplateGallery";

vi.mock("@/api/v2/request", async () => (await import("@/test/v2Recorder")).mockV2Request());
vi.mock("sonner", () => ({ toast: { success: vi.fn(), warning: vi.fn(), error: vi.fn() } }));

installV2Recorder();

const libraries = [
  { id: 1, name: "Movies", type: "movies" },
  { id: 2, name: "Shows", type: "series" },
] as Library[];

const template = (id: string, title: string, mediaKind: "movie" | "tv") => ({
  id,
  title,
  description: `${title} on TMDB.`,
  icon: "🎬",
  category: "trending",
  source: "tmdb",
  media_kind: mediaKind,
  tmdb: { preset: "trending", media_type: mediaKind, time_window: "week" },
});

beforeEach(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  HTMLElement.prototype.scrollIntoView = () => {};
  HTMLElement.prototype.hasPointerCapture = () => false;
  v2Recorder.answer("GET /api/v2/admin/collections/capabilities", adminCapabilities);
  v2Recorder.answer("GET /api/v2/admin/collections/templates", {
    categories: [
      {
        category: "trending",
        label: "Trending",
        templates: [
          template("tmdb_trending_movies_week", "Trending Movies This Week", "movie"),
          template("tmdb_trending_tv_week", "Trending Shows This Week", "tv"),
        ],
      },
    ],
  });
  v2Recorder.answer("GET /api/v2/admin/collections/template-bundles", {
    bundles: [
      {
        id: "core_defaults",
        title: "Core Defaults",
        description: "A focused starter set of movie and TV collections.",
        template_ids: ["tmdb_trending_movies_week", "tmdb_trending_tv_week"],
      },
    ],
  });
  v2Recorder.answer("POST /api/v2/admin/collections/template-bundles/{bundle_id}/apply", {
    ...getAdminTemplateJobCompleted.template_result,
    dry_run: true,
  });
});

async function openBundle() {
  const user = userEvent.setup();
  render(
    <QueryClientProvider
      client={
        new QueryClient({
          defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
        })
      }
    >
      <CollectionTemplateGallery
        open
        onOpenChange={vi.fn()}
        libraries={libraries}
        initialLibraryId={null}
      />
    </QueryClientProvider>,
  );
  await user.click(await screen.findByText("Core Defaults"));
  return user;
}

async function turnHeroesOff(user: ReturnType<typeof userEvent.setup>) {
  const select = async (current: string, choice: string) => {
    const trigger = screen
      .getAllByRole("combobox")
      .find((element) => element.textContent === current);
    if (!trigger) throw new Error(`No hero select shows "${current}"`);
    await user.click(trigger);
    await user.click(await screen.findByRole("option", { name: choice }));
  };
  await select("Movies / Trending Movies This Week", "No home hero");
  await select("Trending Movies This Week", "No library hero");
  await select("Trending Shows This Week", "No library hero");
}

describe("template bundle apply", () => {
  it("previews with the default hero sections", async () => {
    const user = await openBundle();
    await user.click(screen.getByRole("button", { name: "Preview" }));
    await screen.findByText(/Would create/);
    expect(v2Recorder.writes()).toEqual(goldens.bundleDryRunWithHeroes);
  });

  it("previews with every hero off and existing collections deleted", async () => {
    const user = await openBundle();
    await turnHeroesOff(user);
    await user.click(screen.getByText("Delete Existing Server Collections"));
    await user.click(screen.getByRole("button", { name: "Preview" }));
    await screen.findByText(/Would create/);
    expect(v2Recorder.writes()).toEqual(goldens.bundleDryRunNoHeroesDeleteExisting);
  });

  it("queues the apply job with the default hero sections", async () => {
    const user = await openBundle();
    await user.click(screen.getByRole("button", { name: "Apply Defaults" }));
    await vi.waitFor(() => expect(v2Recorder.writes()).toHaveLength(1));
    expect(v2Recorder.writes()).toEqual(goldens.bundleJobWithHeroes);
  });

  it("queues the apply job with every hero off", async () => {
    const user = await openBundle();
    await turnHeroesOff(user);
    await user.click(screen.getByRole("button", { name: "Apply Defaults" }));
    await vi.waitFor(() => expect(v2Recorder.writes()).toHaveLength(1));
    expect(v2Recorder.writes()).toEqual(goldens.bundleJobNoHeroes);
  });

  it("queues the apply job with existing collections deleted", async () => {
    const user = await openBundle();
    await user.click(screen.getByText("Delete Existing Server Collections"));
    await user.click(screen.getByRole("button", { name: "Apply Defaults" }));
    await vi.waitFor(() => expect(v2Recorder.writes()).toHaveLength(1));
    expect(v2Recorder.writes()).toEqual(goldens.bundleJobDeleteExisting);
  });
});
