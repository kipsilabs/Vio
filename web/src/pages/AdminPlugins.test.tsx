import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest";

import { stubRadixDom } from "./adminPage.test-support";
import AdminPlugins from "./AdminPlugins";

const useAdminPluginsMock = vi.fn();
const refetchInstallationsMock = vi.fn();
const refetchCatalogMock = vi.fn();

vi.mock("@tanstack/react-query", async () => {
  const actual =
    await vi.importActual<typeof import("@tanstack/react-query")>("@tanstack/react-query");
  return {
    ...actual,
    useQueryClient: () => ({ invalidateQueries: vi.fn() }),
  };
});

vi.mock("@/hooks/queries/admin/plugins", () => ({
  CHECK_PLUGIN_UPDATES_TASK_KEY: "check_plugin_updates",
  useAdminPlugins: () => useAdminPluginsMock(),
  useAdminPluginProvisioningStatus: () => ({
    data: [],
    isError: false,
    isFetching: false,
    refetch: vi.fn(),
  }),
  useCheckPluginUpdates: () => ({ mutate: vi.fn(), isPending: false }),
  useUpdatePluginCatalogSettings: () => ({ mutate: vi.fn(), isPending: false }),
  useCreatePluginRepository: () => ({ mutate: vi.fn(), isPending: false }),
  useUpdatePluginRepository: () => ({ mutate: vi.fn(), isPending: false }),
  useDeletePluginRepository: () => ({ mutate: vi.fn(), isPending: false }),
  useInstallPlugin: () => ({ mutate: vi.fn(), isPending: false }),
  useUploadPlugin: () => ({ mutate: vi.fn(), isPending: false }),
  usePluginUpload: () => ({ upload: vi.fn(), progress: null, isPending: false }),
  useUpdatePluginInstallation: () => ({ mutate: vi.fn(), isPending: false }),
  useApplyPluginUpdate: () => ({ mutate: vi.fn(), isPending: false }),
  useRestartPluginInstallation: () => ({ mutate: vi.fn(), isPending: false }),
  useDeletePluginInstallation: () => ({ mutate: vi.fn(), isPending: false }),
  useSavePluginConfig: () => ({ mutate: vi.fn(), isPending: false }),
  useTestPluginConfig: () => ({ mutate: vi.fn(), isPending: false }),
  useSavePluginAuthBinding: () => ({ mutate: vi.fn(), isPending: false }),
  useSavePluginTaskBinding: () => ({ mutate: vi.fn(), isPending: false }),
}));

vi.mock("@/hooks/queries/admin/tasks", () => ({
  useTask: () => ({ data: { key: "check_plugin_updates", state: "idle" } }),
}));

function plugins(overrides: Record<string, unknown> = {}) {
  return {
    repositories: [],
    repositoriesError: null,
    catalog: [],
    catalogError: null,
    installations: [],
    installationsError: null,
    catalogSettings: undefined,
    isLoading: false,
    isFetching: false,
    refetchCatalog: refetchCatalogMock,
    refetchInstallations: refetchInstallationsMock,
    ...overrides,
  };
}

function renderPage(path = "/admin/plugins") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AdminPlugins />
    </MemoryRouter>,
  );
}

describe("AdminPlugins load failures", () => {
  beforeAll(stubRadixDom);

  beforeEach(() => {
    refetchInstallationsMock.mockReset();
    refetchCatalogMock.mockReset();
    useAdminPluginsMock.mockReturnValue(plugins());
  });

  it("reports a failed installed-plugin read instead of the empty state", () => {
    useAdminPluginsMock.mockReturnValue(
      plugins({ installationsError: new Error("Unrecognized plugin source kind.") }),
    );

    renderPage();

    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent("Couldn't load plugins");
    expect(alert).toHaveTextContent("Unrecognized plugin source kind.");
    expect(screen.queryByText("No plugins installed")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(refetchInstallationsMock).toHaveBeenCalledTimes(1);
  });

  it("reports a failed catalog read instead of the empty state", () => {
    useAdminPluginsMock.mockReturnValue(
      plugins({ catalogError: new Error("Unrecognized plugin source kind.") }),
    );

    renderPage("/admin/plugins?tab=catalog");

    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent("Couldn't load the plugin catalog");
    expect(alert).toHaveTextContent("Unrecognized plugin source kind.");
    expect(screen.queryByText("No plugins available")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(refetchCatalogMock).toHaveBeenCalledTimes(1);
  });

  it.each([
    ["installed", "/admin/plugins", "No plugins installed"],
    ["catalog", "/admin/plugins?tab=catalog", "No plugins available"],
  ])("keeps the %s empty state when the list loads empty", (_tab, path, emptyText) => {
    renderPage(path);

    expect(screen.getByText(emptyText)).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});
