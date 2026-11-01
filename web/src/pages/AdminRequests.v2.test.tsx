// @vitest-environment jsdom
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router";
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
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
const mockAdminPluginInstallations = vi.fn(() => ({
  data: [{ id: 1, plugin_id: "router", capabilities: [{ type: "request_router.v1", id: "arr" }] }],
  isLoading: false,
}));
vi.mock("@/hooks/queries/admin/plugins", () => ({
  useAdminPluginInstallations: () => mockAdminPluginInstallations(),
}));
vi.mock("@/hooks/queries/admin/users", () => ({
  useAdminUsers: () => ({ data: [{ id: 1, username: "member" }], isLoading: false }),
}));
const settings = {
  requests_enabled: true,
  global_max_requests: 5,
  global_window_days: 7,
  global_auto_approval_enabled: false,
  force_dual_quality: false,
};
const conflict = () =>
  new V2ProblemError(
    "updateAdminRequestSettings",
    {
      type: "https://silo.test/problems/precondition_failed",
      title: "Changed",
      status: 412,
      detail: "Changed",
      instance: "test",
    },
    null,
    '"newer"',
  );
function reply(options: unknown, body: unknown, etag = '"initial"') {
  (options as { onResponse?: (r: Response) => void })?.onResponse?.(
    new Response(null, { headers: { ETag: etag } }),
  );
  return Promise.resolve(body) as never;
}

function LocationProbe() {
  const location = useLocation();
  return <output data-testid="location">{location.search}</output>;
}
function mount(tab: string) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: 3 } },
  });
  const path = tab.startsWith("/") ? tab : `/admin/requests?tab=${tab}`;
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
window.HTMLElement.prototype.scrollIntoView ??= () => {};
window.HTMLElement.prototype.hasPointerCapture ??= () => false;

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("request administration", () => {
  it.each(["integrations"])(
    "sends the retired ?tab=%s to the Requests settings page",
    async (tab) => {
      serve({ "GET /api/v2/admin/requests/capabilities": capabilities });
      mount(`/admin/requests?tab=${tab}`);
      expect(
        await screen.findByRole("heading", { name: "Request settings page" }),
      ).toBeInTheDocument();
    },
  );

  it("sends the retired ?tab=overrides to the accounts list and says where limits went", async () => {
    serve({ "GET /api/v2/admin/requests/capabilities": capabilities });
    mount("/admin/requests?tab=overrides");
    expect(await screen.findByRole("heading", { name: "Users page" })).toBeInTheDocument();
    expect(toast.info).toHaveBeenCalledWith(
      "Request limits moved to each account",
      expect.objectContaining({ id: "request-overrides-moved" }),
    );
    expect(calls("GET /api/v2/admin/request-users/{user_id}/limit")).toHaveLength(0);
  });

  it("opens on In progress when nothing needs approval, and shows each view's count", async () => {
    serve(
      queue(
        { in_progress: [request("r2", "Approved Title", { status: "approved" })] },
        { needs_approval: 0, in_progress: 2, failed: 1, done: 3 },
      ),
    );
    mount("/admin/requests");
    expect(await rowOf("Approved Title")).toBeInTheDocument();
    expect(screen.getByTestId("location")).toHaveTextContent("view=in_progress");
    const views = screen.getByRole("tablist", { name: "Request views" });
    expect(
      within(views)
        .getAllByRole("tab")
        .map((tab) => tab.textContent),
    ).toEqual(["Needs approval 0", "In progress 2", "Failed 1", "Done 3"]);
    expect(screen.getByRole("tab", { name: "In progress 2" })).toHaveAttribute(
      "aria-selected",
      "true",
    );
    expect(calls("GET /api/v2/admin/requests").map((options) => options.query?.view)).toEqual([
      "in_progress",
    ]);
  });

  it("opens on Needs approval while something waits, and keeps the view a link names", async () => {
    serve(queue({ needs_approval: [request("r1", "Waiting Title")] }));
    mount("/admin/requests");
    expect(await rowOf("Waiting Title")).toBeInTheDocument();
    expect(screen.getByTestId("location")).toHaveTextContent("view=needs_approval");
    cleanup();

    serve(queue({ failed: [request("r4", "Broken Title", { outcome: "failed" })] }));
    mount("/admin/requests?view=failed");
    expect(await rowOf("Broken Title")).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Failed 1" })).toHaveAttribute("aria-selected", "true");
  });

  it("offers only the actions the server allows in each view", async () => {
    serve(
      queue({
        needs_approval: [request("r1", "Waiting Title")],
        in_progress: [
          request("r2", "Unsent Title", { status: "approved", state: "approved" }),
          request("r3", "Sent Title", {
            status: "queued",
            state: "processing",
            targets: [target("t3")],
          }),
        ],
        failed: [
          request("r4", "Broken Title", {
            status: "approved",
            outcome: "failed",
            state: "failed",
            last_error: "no fulfillment backend configured",
          }),
        ],
        done: [request("r5", "Finished Title", { status: "completed", state: "available" })],
      }),
    );
    mount("/admin/requests?view=needs_approval");
    expect(buttonNames(await rowOf("Waiting Title"))).toEqual([
      "Approve: Waiting Title",
      "Decline: Waiting Title",
    ]);

    fireEvent.mouseDown(screen.getByRole("tab", { name: /^In progress/ }));
    expect(buttonNames(await rowOf("Unsent Title"))).toEqual(["Cancel request: Unsent Title"]);
    expect(buttonNames(await rowOf("Sent Title"))).toEqual([]);

    fireEvent.mouseDown(screen.getByRole("tab", { name: /^Failed/ }));
    const broken = await rowOf("Broken Title");
    // An admin retries a failed request or closes it.
    expect(buttonNames(broken)).toEqual(["Retry: Broken Title", "Cancel request: Broken Title"]);
    expect(within(broken).getByText("no fulfillment backend configured")).toBeInTheDocument();

    fireEvent.mouseDown(screen.getByRole("tab", { name: /^Done/ }));
    expect(buttonNames(await rowOf("Finished Title"))).toEqual([]);
  });

  it("declines with the reason given and refreshes the counts", async () => {
    const handlers = queue({ needs_approval: [request("r1", "Waiting Title")] });
    serve({
      ...handlers,
      "POST /api/v2/admin/requests/{id}/decline": ({ path }) =>
        request(path!.id!, "Waiting Title", { outcome: "declined", state: "declined" }),
    });
    mount("/admin/requests?view=needs_approval");
    fireEvent.click(within(await rowOf("Waiting Title")).getByRole("button", { name: /^Decline/ }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText("Reason (optional)"), {
      target: { value: "  Already in the library  " },
    });
    const countsBefore = calls("GET /api/v2/admin/requests/counts").length;
    fireEvent.click(within(dialog).getByRole("button", { name: "Decline" }));
    await waitFor(() =>
      expect(calls("POST /api/v2/admin/requests/{id}/decline")).toEqual([
        expect.objectContaining({ path: { id: "r1" }, body: { reason: "Already in the library" } }),
      ]),
    );
    await waitFor(() =>
      expect(calls("GET /api/v2/admin/requests/counts").length).toBeGreaterThan(countsBefore),
    );
  });

  it("shows a spinner on the running action until the server answers", async () => {
    const failed = request("r4", "Broken Title", {
      status: "approved",
      outcome: "failed",
      state: "failed",
    });
    let finish: (() => void) | undefined;
    serve({
      ...queue({ failed: [failed] }),
      "POST /api/v2/admin/requests/{id}/retry": ({ path }) =>
        new Promise((resolve) => {
          finish = () => resolve(request(path!.id!, "Broken Title", { status: "queued" }));
        }),
    });
    mount("/admin/requests?view=failed");
    const row = await rowOf("Broken Title");
    const retry = within(row).getByRole("button", { name: "Retry: Broken Title" });
    const cancel = within(row).getByRole("button", { name: "Cancel request: Broken Title" });
    expect(retry).not.toHaveAttribute("aria-busy");

    fireEvent.click(retry);
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));
    // The row's other actions wait too, without a spinner of their own.
    expect(retry).toBeDisabled();
    expect(cancel).toBeDisabled();
    expect(cancel).not.toHaveAttribute("aria-busy");

    await act(async () => finish!());
    await waitFor(() => expect(retry).not.toHaveAttribute("aria-busy"));
    expect(retry).toBeEnabled();
  });

  it("approves in bulk four at a time and reports the ones that failed", async () => {
    const rows = Array.from({ length: 6 }, (_, i) => request(`p${i + 1}`, `Title ${i + 1}`));
    const pending = new Map<string, { resolve: () => void; reject: (err: unknown) => void }>();
    serve({
      ...queue({ needs_approval: rows }, { ...COUNTS, needs_approval: 6 }),
      "POST /api/v2/admin/requests/{id}/approve": ({ path }) =>
        new Promise((resolve, reject) => {
          const id = path!.id!;
          pending.set(id, {
            resolve: () => resolve(request(id, id, { status: "approved", state: "approved" })),
            reject,
          });
        }),
    });
    mount("/admin/requests?view=needs_approval");
    await rowOf("Title 6");
    fireEvent.click(screen.getByLabelText("Select all 6 shown"));
    const countsBefore = calls("GET /api/v2/admin/requests/counts").length;
    fireEvent.click(screen.getByRole("button", { name: "Approve 6" }));

    await waitFor(() => expect(pending.size).toBe(4));
    expect(screen.getByText(/Approving 0 of 6/)).toBeInTheDocument();
    // Row actions wait while the bulk action runs.
    expect(
      within(await rowOf("Title 1")).getByRole("button", { name: "Approve: Title 1" }),
    ).toBeDisabled();

    await act(async () => pending.get("p1")!.resolve());
    await waitFor(() => expect(pending.size).toBe(5));
    await act(async () => {
      pending.get("p2")!.resolve();
      pending.get("p3")!.reject(
        new V2ProblemError("adminApproveRequest", {
          type: "https://silo.test/problems/conflict",
          title: "Conflict",
          status: 409,
          detail: "The request is no longer pending.",
          instance: "test",
        }),
      );
      pending.get("p4")!.resolve();
    });
    await waitFor(() => expect(pending.size).toBe(6));
    await act(async () => {
      pending.get("p5")!.resolve();
      pending.get("p6")!.resolve();
    });

    const report = await screen.findByRole("alert");
    expect(report).toHaveTextContent("5 of 6 approved; 1 couldn't be.");
    expect(report).toHaveTextContent("Title 3: The request is no longer pending.");
    expect(calls("POST /api/v2/admin/requests/{id}/approve")).toHaveLength(6);
    await waitFor(() =>
      expect(calls("GET /api/v2/admin/requests/counts").length).toBeGreaterThan(countsBefore),
    );
    // The failed one stays selected, ready to try again.
    expect(screen.getByRole("checkbox", { name: "Select Title 3" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "Select Title 1" })).not.toBeChecked();
  });

  it("shows the refetched request in the open dialog after another admin acted first", async () => {
    const LATER = "2026-09-02T00:00:00Z";
    let sent = false;
    serve({
      ...queue({}),
      "GET /api/v2/admin/requests": ({ query }) => {
        if (query?.view === "in_progress") {
          return page([
            sent
              ? request("r2", "Unsent Title", {
                  status: "queued",
                  state: "processing",
                  targets: [target("t2")],
                  updated_at: LATER,
                })
              : request("r2", "Unsent Title", { status: "approved", state: "approved" }),
          ]);
        }
        return page(
          query?.view === "needs_approval" && !sent ? [request("r1", "Waiting Title")] : [],
        );
      if (operation === "PUT /api/v2/admin/request-settings") return Promise.reject(conflict());
      throw new Error(operation);
    });
    mount("settings");
    await screen.findByText("Save Settings");
    const staleTopInput = screen.getAllByRole("spinbutton")[0]!;
    fireEvent.change(staleTopInput, { target: { value: "13" } });
    fireEvent.click(screen.getByText("Save Settings"));
    await screen.findByRole("alert");
    cleanup();
  });
  it("keeps deletion confirmation open after a stale integration delete", async () => {
    const row = {
      id: "integration",
      name: "Connection",
      enabled: true,
      base_url: "https://example.invalid",
      has_api_key: true,
      installation_id: "1",
      capability_id: "arr",
      plugin_config: {},
      supported_media_types: [],
      last_check_at: null,
      last_check_status: "",
      last_check_error: "",
      updated_at: "2026-09-05T00:00:00Z",
    };
    vi.mocked(v2).mockImplementation((operation, options) => {
      if (operation === "GET /api/v2/admin/requests/capabilities")
        return reply(options, { available: true, guarded_configuration: true });
      if (operation === "GET /api/v2/admin/request-integrations")
        return reply(options, { items: [row], page: { has_more: false } });
      if (operation === "GET /api/v2/admin/request-integrations/{id}") return reply(options, row);
      if (operation === "DELETE /api/v2/admin/request-integrations/{id}")
        return Promise.reject(conflict());
      if (operation === "POST /api/v2/admin/request-integrations/{id}/options")
        return reply(options, { options: {} });
      throw new Error(operation);
    });
    mount("integrations");
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    const staleDialog = await screen.findByRole("dialog");
    fireEvent.click(within(staleDialog).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(within(staleDialog).getByRole("alert")).toBeTruthy());
    expect(screen.getByRole("dialog")).toBeTruthy();
    const writes = vi
      .mocked(v2)
      .mock.calls.filter(([op]) => op === "DELETE /api/v2/admin/request-integrations/{id}");
    expect(writes).toHaveLength(1);
    expect(writes[0]![1]!).toMatchObject({ headers: { "If-Match": '"initial"' } });
  });
  it("renders generic integration field errors inline and clears them on edit", async () => {
    const row = {
      id: "integration",
      name: "Connection",
      enabled: true,
      base_url: "https://example.invalid",
      has_api_key: true,
      installation_id: "1",
      capability_id: "arr",
      plugin_config: {},
      supported_media_types: [],
      last_check_at: null,
      last_check_status: "",
      last_check_error: "",
      updated_at: "2026-09-05T00:00:00Z",
    };
    const errors = [
      { location: "body.name", code: "invalid", detail: "Connection name is rejected" },
      { location: "body.api_key_ref", code: "invalid", detail: "Re-enter the credential" },
      { location: "body.base_url", code: "invalid", detail: "Server URL is rejected" },
      { location: "body.installation_id", code: "invalid", detail: "Choose another installation" },
    ];
    vi.mocked(v2).mockImplementation((operation, options) => {
      if (operation === "GET /api/v2/admin/requests/capabilities")
        return reply(options, { available: true, guarded_configuration: true });
      if (operation === "GET /api/v2/admin/request-integrations")
        return reply(options, { items: [row], page: { has_more: false } });
      if (operation === "GET /api/v2/admin/request-integrations/{id}") return reply(options, row);
      if (operation === "POST /api/v2/admin/request-integrations/{id}/options")
        return reply(options, { options: {} });
      if (operation === "PUT /api/v2/admin/request-integrations/{id}")
        return Promise.reject(
          new V2ProblemError("updateRequestIntegration", {
            type: "https://example.invalid/problems/validation_failed",
            title: "Invalid",
            status: 422,
            detail: "Review invalid fields",
            instance: "test",
            errors,
          }),
        );
      throw new Error(operation);
    });
    mount("integrations");
    fireEvent.click(await screen.findByRole("button", { name: "Save" }));
    for (const error of errors) expect(await screen.findByText(error.detail)).toBeTruthy();
    const name = screen.getByPlaceholderText("Connection name");
    expect(name.getAttribute("aria-invalid")).toBe("true");
    expect(name.parentElement?.textContent).toContain("Connection name is rejected");
    fireEvent.change(name, { target: { value: "Corrected" } });
    expect(screen.queryByText("Connection name is rejected")).toBeNull();
    expect(name.getAttribute("aria-invalid")).toBe("false");
  });
  it("keeps user override edits and validator until explicit reload after a stale response", async () => {
    let reads = 0;
    vi.mocked(v2).mockImplementation((operation, options) => {
      if (operation === "GET /api/v2/admin/requests/capabilities")
        return reply(options, { available: true, guarded_configuration: true });
      if (operation === "GET /api/v2/admin/request-users/{user_id}/limit")
        return reply(
          options,
          {
            user_id: "1",
            limit_mode: "custom",
            max_requests: ++reads === 1 ? 3 : 6,
            window_days: 7,
            approval_mode: "inherit",
          },
          reads === 1 ? '"initial"' : '"reloaded"',
        );
      if (operation === "PUT /api/v2/admin/request-users/{user_id}/limit")
        return Promise.reject(conflict());
      throw new Error(operation);
    });
    mount("overrides");
    await screen.findByText("Save Override");
    const input = screen.getAllByRole("spinbutton")[0]!;
    fireEvent.change(input, { target: { value: "11" } });
    fireEvent.click(screen.getByText("Save Override"));
    await screen.findByRole("alert");
    expect((input as HTMLInputElement).value).toBe("11");
    expect(
      vi
        .mocked(v2)
        .mock.calls.filter(([op]) => op === "PUT /api/v2/admin/request-users/{user_id}/limit"),
    ).toHaveLength(1);
    const writes = () =>
      vi
        .mocked(v2)
        .mock.calls.filter(([op]) => op === "PUT /api/v2/admin/request-users/{user_id}/limit");
    expect(writes()[0]![1]).toMatchObject({
      headers: { "If-Match": '"initial"' },
      body: { max_requests: 11 },
    });
    expect(
      (screen.getByText("Save Override").closest("button") as HTMLButtonElement).disabled,
    ).toBe(true);
    fireEvent.click(screen.getByText("Reload latest version"));
    await waitFor(() =>
      expect((screen.getAllByRole("spinbutton")[0] as HTMLInputElement).value).toBe("6"),
    );
    fireEvent.click(screen.getByText("Save Override"));
    await waitFor(() => expect(writes()).toHaveLength(2));
    expect(writes()[1]![1]).toMatchObject({
      headers: { "If-Match": '"reloaded"' },
      body: { max_requests: 6 },
    });
  });
  it("populates virtual://streaming and creates virtual library connection", async () => {
    mockAdminPluginInstallations.mockReturnValueOnce({
      data: [],
      isLoading: false,
    });
    vi.mocked(v2).mockImplementation((operation, options) => {
      if (operation === "GET /api/v2/admin/requests/capabilities")
        return reply(options, { available: true, guarded_configuration: true });
      if (operation === "GET /api/v2/admin/request-integrations")
        return reply(options, { items: [], page: { has_more: false } });
      if (operation === "POST /api/v2/admin/request-integrations") {
        return reply(options, {
          id: "conn-virt",
          name: "Virtual Library (Core Streaming)",
          enabled: true,
          base_url: "virtual://streaming",
          has_api_key: true,
          installation_id: "0",
          capability_id: "virtual-library-requests",
          plugin_config: {},
          supported_media_types: [],
          last_check_at: null,
          last_check_status: "",
          last_check_error: "",
          updated_at: "2026-10-02T00:00:00Z",
        });
      }
      if (operation === "POST /api/v2/admin/request-integrations/{id}/options")
        return reply(options, { options: {} });
      throw new Error(operation);
    });
    mount("integrations");
    fireEvent.click(await screen.findByRole("button", { name: "Add connection" }));

    const nameInput = (await screen.findByPlaceholderText("Connection name")) as HTMLInputElement;
    expect(nameInput.value).toBe("Virtual Library (Core Streaming)");
    expect((screen.getByPlaceholderText("virtual://streaming") as HTMLInputElement).value).toBe(
      "virtual://streaming",
    );
    expect((screen.getByPlaceholderText("core-managed") as HTMLInputElement).value).toBe(
      "core-managed",
    );

    const createBtn = screen.getByRole("button", { name: "Create connection" });
    fireEvent.click(createBtn);
    await waitFor(() => {
      const creates = vi
        .mocked(v2)
        .mock.calls.filter(([op]) => op === "POST /api/v2/admin/request-integrations");
      expect(creates).toHaveLength(1);
      expect(creates[0]![1]!).toMatchObject({
        body: {
          name: "Virtual Library (Core Streaming)",
          base_url: "virtual://streaming",
          api_key_ref: "core-managed",
          installation_id: "0",
          capability_id: "virtual-library-requests",
        },
      });
    });
  });
  it("populates virtual defaults when selecting virtual library plugin from multiple plugins", async () => {
    vi.mocked(v2).mockImplementation((operation, options) => {
      if (operation === "GET /api/v2/admin/requests/capabilities")
        return reply(options, { available: true, guarded_configuration: true });
      if (operation === "GET /api/v2/admin/request-integrations")
        return reply(options, { items: [], page: { has_more: false } });
      if (operation === "POST /api/v2/admin/request-integrations") {
        return reply(options, {
          id: "conn-virt-2",
          name: "Virtual Library (Core Streaming)",
          enabled: true,
          base_url: "virtual://streaming",
          has_api_key: true,
          installation_id: "0",
          capability_id: "virtual-library-requests",
          plugin_config: {},
          supported_media_types: [],
          last_check_at: null,
          last_check_status: "",
          last_check_error: "",
          updated_at: "2026-10-02T00:00:00Z",
        });
      }
      if (operation === "POST /api/v2/admin/request-integrations/{id}/options")
        return reply(options, { options: {} });
      throw new Error(operation);
    });
    mount("integrations");
    fireEvent.click(await screen.findByRole("button", { name: "Add connection" }));

    expect(screen.getByText("Select a plugin to configure this connection.")).toBeInTheDocument();

    fireEvent.keyDown(screen.getByRole("combobox"), { key: "ArrowDown" });
    fireEvent.click(
      await screen.findByRole("option", {
        name: "Virtual Library (Core Streaming) (virtual-library-requests)",
      }),
    );

    const nameInput = (await screen.findByPlaceholderText("Connection name")) as HTMLInputElement;
    expect(nameInput.value).toBe("Virtual Library (Core Streaming)");
    expect((screen.getByPlaceholderText("virtual://streaming") as HTMLInputElement).value).toBe(
      "virtual://streaming",
    );
    expect((screen.getByPlaceholderText("core-managed") as HTMLInputElement).value).toBe(
      "core-managed",
    );

    const createBtn = screen.getByRole("button", { name: "Create connection" });
    fireEvent.click(createBtn);
    await waitFor(() => {
      const creates = vi
        .mocked(v2)
        .mock.calls.filter(([op]) => op === "POST /api/v2/admin/request-integrations");
      expect(creates).toHaveLength(1);
      expect(creates[0]![1]!).toMatchObject({
        body: {
          name: "Virtual Library (Core Streaming)",
          base_url: "virtual://streaming",
          api_key_ref: "core-managed",
          installation_id: "0",
          capability_id: "virtual-library-requests",
        },
      });
    });
  });
});
