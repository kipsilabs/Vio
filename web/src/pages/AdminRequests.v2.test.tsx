// @vitest-environment jsdom
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router";
import { toast } from "sonner";
import { afterEach, describe, expect, it, vi } from "vitest";
import { v2, V2ProblemError } from "@/api/v2/request";
import AdminRequests from "./AdminRequests";

vi.mock("@/api/v2/request", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/v2/request")>()),
  v2: vi.fn(),
}));
vi.mock("@/api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/client")>()),
  captureProfileRequestContext: () => ({
    accessToken: "test",
    profileId: "profile",
    profileToken: null,
    authContextVersion: 1,
    serverOrigin: "",
  }),
  isProfileRequestContextCurrent: () => true,
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() } }));
vi.mock("@/hooks/queries/admin/users", () => ({
  useAdminUsers: () => ({
    data: [
      { id: 1, username: "member" },
      { id: 7, username: "sam" },
    ],
    isLoading: false,
  }),
}));

const CREATED = "2026-09-01T00:00:00Z";
function reply(options: unknown, body: unknown, etag = '"initial"') {
  (options as { onResponse?: (r: Response) => void })?.onResponse?.(
    new Response(null, { headers: { ETag: etag } }),
  );
  return Promise.resolve(body) as never;
}

type Options = {
  query?: Record<string, unknown>;
  path?: Record<string, string>;
  body?: Record<string, unknown>;
};
type Handler = (options: Options) => unknown;

/** Answers each v2 operation from `handlers`; any other operation fails. */
function serve(handlers: Record<string, Handler>) {
  vi.mocked(v2).mockImplementation((operation, options) => {
    const handler = handlers[operation];
    if (!handler) return Promise.reject(new Error(`unexpected ${operation}`)) as never;
    return reply(options, handler((options ?? {}) as Options));
  });
}
function calls(operation: string) {
  return vi
    .mocked(v2)
    .mock.calls.filter(([op]) => op === operation)
    .map(([, options]) => options as Options);
}

function request(id: string, title: string, fields: Record<string, unknown> = {}) {
  return {
    id,
    provider: "tmdb",
    media_type: "movie",
    tmdb_id: Number(id.replace(/\D/g, "")) || 1,
    title,
    status: "pending",
    outcome: "active",
    state: "pending",
    requested_by_user_id: "1",
    is_anime: false,
    seasons: [],
    season_progress: [],
    targets: [],
    created_at: CREATED,
    updated_at: CREATED,
    ...fields,
  };
}
const target = (id: string, fields: Record<string, unknown> = {}) => ({
  id,
  request_id: "r",
  quality: "1080p",
  is_anime: false,
  status: "queued",
  instance_name: "Radarr",
  route_name: "Everything else",
  created_at: CREATED,
  updated_at: CREATED,
  ...fields,
});
const COUNTS = { needs_approval: 1, in_progress: 2, failed: 1, done: 1 };
const capabilities = () => ({ available: true, guarded_configuration: true });
const page = (items: unknown[]) => ({ items, page: { has_more: false } });

/** One queue per view, as the server filters it. */
function queue(byView: Record<string, unknown[]>, counts = COUNTS): Record<string, Handler> {
  return {
    "GET /api/v2/admin/requests/capabilities": capabilities,
    "GET /api/v2/admin/requests/counts": () => counts,
    "GET /api/v2/admin/requests": ({ query }) => page(byView[String(query?.view)] ?? []),
  };
}

function LocationProbe() {
  const location = useLocation();
  return <output data-testid="location">{location.search}</output>;
}
function mount(path: string) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: 3 } },
  });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route
            path="/admin/requests"
            element={
              <>
                <AdminRequests />
                <LocationProbe />
              </>
            }
          />
          <Route path="/admin/settings/requests" element={<h1>Request settings page</h1>} />
          <Route path="/admin/users" element={<h1>Users page</h1>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return client;
}
const rowOf = async (title: string) =>
  (await screen.findByRole("link", { name: title })).closest("li") as HTMLElement;
const buttonNames = (row: HTMLElement) =>
  within(row)
    .getAllByRole("button")
    .map((button) => button.getAttribute("aria-label") ?? button.textContent)
    .filter((name) => !name?.startsWith("Details") && name !== "member");

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("request administration", () => {
  it("retains settings and original validator across background refresh and 412, then explicitly reloads", async () => {
    let reads = 0;
    vi.mocked(v2).mockImplementation((operation, options) => {
      if (operation === "GET /api/v2/admin/requests/capabilities")
        return reply(options, { available: true, guarded_configuration: true });
      if (operation === "GET /api/v2/admin/request-settings")
        return reply(
          options,
          { ...settings, global_max_requests: ++reads === 1 ? 5 : 9 },
          reads === 1 ? '"initial"' : '"reloaded"',
        );
      },
      "GET /api/v2/admin/requests/{id}/events": () => ({ items: [] }),
      "POST /api/v2/admin/request-routes/preview": () => ({
        facts: {
          anime: false,
          company_ids: [],
          genre_ids: [],
          keyword_ids: [],
          network_ids: [],
          origin_countries: [],
        },
        rules: [],
        tiers: [],
      }),
      // Another admin sent the one and approved the other a moment ago.
      "POST /api/v2/admin/requests/{id}/cancel": () => {
        sent = true;
        return Promise.reject(new Error("The request changed; reload it."));
      },
      "POST /api/v2/admin/requests/{id}/approve": () => {
        sent = true;
        return Promise.reject(new Error("The request changed; reload it."));
      },
    });
    mount("/admin/requests?view=in_progress");
    fireEvent.click(
      within(await rowOf("Unsent Title")).getByRole("button", { name: "Details: Unsent Title" }),
    );
    let dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel request: Unsent Title" }));
    const prompt = (await screen.findAllByRole("dialog")).find((d) => d !== dialog)!;
    fireEvent.click(within(prompt).getByRole("button", { name: "Cancel request" }));
    // The refused cancel refetched the queue; the dialog shows the sent
    // request, which can no longer be cancelled.
    await waitFor(() =>
      expect(
        within(dialog).queryByRole("button", { name: "Cancel request: Unsent Title" }),
      ).not.toBeInTheDocument(),
    );
    expect(within(dialog).getByRole("table")).toHaveTextContent("Radarr");

    // A request that left the view closes its dialog.
    fireEvent.click(within(dialog).getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    sent = false;
    fireEvent.mouseDown(screen.getByRole("tab", { name: /^Needs approval/ }));
    fireEvent.click(
      within(await rowOf("Waiting Title")).getByRole("button", { name: "Details: Waiting Title" }),
    );
    dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Approve: Waiting Title" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(screen.queryByRole("link", { name: "Waiting Title" })).not.toBeInTheDocument();
  });

  it("loads the next page from the cursor the last one returned", async () => {
    serve({
      ...queue({}),
      "GET /api/v2/admin/requests": ({ query }) =>
        query?.cursor === "c2"
          ? page([request("r2", "Second Page Title")])
          : {
              items: [request("r1", "First Page Title")],
              page: { has_more: true, next_cursor: "c2" },
            },
    });
    mount("/admin/requests?view=needs_approval");
    await rowOf("First Page Title");
    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await rowOf("Second Page Title")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "First Page Title" })).toBeInTheDocument();
    expect(calls("GET /api/v2/admin/requests").map((options) => options.query?.cursor)).toEqual([
      undefined,
      "c2",
    ]);
    expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
  });

  it("limits the queue to one account from ?user= and searches as the admin types", async () => {
    serve(queue({ in_progress: [request("r2", "Approved Title", { status: "approved" })] }));
    mount("/admin/requests?view=in_progress&user=7");
    await rowOf("Approved Title");
    expect(screen.getByText("Requested by sam")).toBeInTheDocument();
    expect(calls("GET /api/v2/admin/requests")[0]?.query).toMatchObject({
      view: "in_progress",
      requested_by_user_id: "7",
    });

    fireEvent.change(screen.getByRole("searchbox", { name: "Search requests" }), {
      target: { value: " dune " },
    });
    fireEvent.click(screen.getByRole("button", { name: "Series" }));
    await waitFor(() =>
      expect(calls("GET /api/v2/admin/requests").at(-1)?.query).toMatchObject({
        q: "dune",
        media_type: "series",
        requested_by_user_id: "7",
      }),
    );

    fireEvent.click(screen.getByRole("button", { name: "Show requests from every account" }));
    await waitFor(() => expect(screen.getByTestId("location")).not.toHaveTextContent("user="));
    await waitFor(() =>
      expect(calls("GET /api/v2/admin/requests").at(-1)?.query).not.toHaveProperty(
        "requested_by_user_id",
        "7",
      ),
    );
  });
});
