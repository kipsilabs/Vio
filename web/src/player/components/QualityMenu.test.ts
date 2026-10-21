// @vitest-environment jsdom

import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { createElement, type ComponentProps } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  buildVersionStatusLabels,
  QualityMenu,
  REFRESH_VERSIONS_ERROR,
  type VersionInfo,
} from "./QualityMenu";
import type { PlayerIndexerRelease } from "../types";
import { REQUEST_RELEASE_ERROR } from "@/hooks/useIndexerReleases";

// The sort preference reads/writes the canonical settings endpoints; the menu
// tests only exercise the display re-order, so it is mocked inert.
const versionSortMock = vi.hoisted(() => ({
  criteria: [] as Array<{ attribute: string; direction: string }>,
  apply: vi.fn(),
  reset: vi.fn(),
}));
vi.mock("@/hooks/useVersionSortPreference", () => ({
  useVersionSortPreference: () => ({
    criteria: versionSortMock.criteria,
    apply: versionSortMock.apply,
    reset: versionSortMock.reset,
    loading: false,
  }),
}));

// The indexer UI is gated on the server capability and the Request action POSTs
// its own endpoint. Mock both so the tests drive the real hooks end to end.
const capabilityMock = vi.hoisted(() => vi.fn());
const requestReleaseMock = vi.hoisted(() => vi.fn());
vi.mock("@/api/v2/virtualLibrary", () => ({
  fetchVirtualLibraryCapability: capabilityMock,
}));
vi.mock("@/api/v2/mediaCandidates", () => ({
  requestVirtualRelease: requestReleaseMock,
}));

beforeEach(() => {
  versionSortMock.criteria = [];
  versionSortMock.apply.mockReset();
  versionSortMock.reset.mockReset();
  capabilityMock.mockReset().mockResolvedValue({
    state: "available",
    indexer_search: true,
    indexer_request: true,
  });
  requestReleaseMock.mockReset();
});

const qualityOptions = [
  {
    id: "original",
    label: "Original",
    sublabel: "25 Mbps",
    resolution: "2160p",
    bitrateKbps: 25_000,
    isOriginal: true,
  },
  {
    id: "1080p-medium",
    label: "1080p Medium",
    sublabel: "6 Mbps",
    resolution: "1080p",
    bitrateKbps: 6000,
    isOriginal: false,
  },
];

function renderVersionMenu(
  overrides: {
    onRefreshVersions?: () => Promise<void>;
    onCancelRefresh?: () => Promise<void> | void;
    onSwitchVersion?: (fileId: number) => void;
    onSelectAutoVersion?: () => void;
    autoFallback?: boolean;
    versions?: VersionInfo[];
    indexerReleases?: PlayerIndexerRelease[];
    contentId?: string;
  } = {},
) {
  render(
    createElement(QualityMenu, {
      options: qualityOptions,
      activeId: "original",
      isTranscoding: false,
      error: null,
      onSelect: () => {},
      versions: overrides.versions ?? [
        makeVersionInfo({ fileId: 1, label: "1080p H264", isCurrentSource: true }),
        makeVersionInfo({ fileId: 2, label: "2160p HEVC" }),
      ],
      indexerReleases: overrides.indexerReleases,
      contentId: overrides.contentId ?? "content-1",
      onSwitchVersion: overrides.onSwitchVersion ?? (() => {}),
      onSelectAutoVersion: overrides.onSelectAutoVersion,
      autoFallback: overrides.autoFallback,
      onRefreshVersions: overrides.onRefreshVersions,
      onCancelRefresh: overrides.onCancelRefresh,
    }),
  );
  fireEvent.click(screen.getByRole("button", { name: "Quality" }));
}

function makeVersionInfo(overrides: Partial<VersionInfo> = {}): VersionInfo {
  return {
    fileId: 1,
    label: "2160p HEVC HDR",
    isCurrentSource: false,
    isRequestedSource: false,
    ...overrides,
  };
}

describe("buildVersionStatusLabels", () => {
  it("shows only Playing when requested and current source match", () => {
    expect(
      buildVersionStatusLabels(
        makeVersionInfo({
          isCurrentSource: true,
          isRequestedSource: true,
        }),
      ),
    ).toEqual(["Playing"]);
  });

  it("shows Playing and Requested on different versions", () => {
    expect(
      buildVersionStatusLabels(
        makeVersionInfo({
          isCurrentSource: true,
        }),
      ),
    ).toEqual(["Playing"]);

    expect(
      buildVersionStatusLabels(
        makeVersionInfo({
          fileId: 2,
          isRequestedSource: true,
        }),
      ),
    ).toEqual(["Requested"]);
  });
});

describe("QualityMenu", () => {
  it("keeps quality adjustments available while the room locks version switching", () => {
    const select = vi.fn();
    const switchVersion = vi.fn();
    render(
      createElement(QualityMenu, {
        options: [
          { id: "auto", label: "Auto", sublabel: "", resolution: "", bitrateKbps: 0 },
          {
            id: "720p",
            label: "720p",
            sublabel: "3 Mbps",
            resolution: "720p",
            bitrateKbps: 3000,
          },
        ],
        activeId: "auto",
        isTranscoding: false,
        error: null,
        onSelect: select,
        onSwitchVersion: switchVersion,
        versionLocked: true,
        versions: [makeVersionInfo(), makeVersionInfo({ fileId: 2, label: "1080p H264" })],
      }),
    );
    fireEvent.click(screen.getByRole("button", { name: "Quality" }));
    expect(screen.getByText(/Watch Party keeps everyone on the same version/)).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: /2160p HEVC HDR/ })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("menuitem", { name: /720p/ }));
    expect(select).toHaveBeenCalledWith("720p");
    expect(switchVersion).not.toHaveBeenCalled();
  });

  it("shows the stored resolution preference as the selected bitrate rung", () => {
    render(
      createElement(QualityMenu, {
        options: [
          { id: "auto", label: "Auto", sublabel: "", resolution: "", bitrateKbps: 0 },
          {
            id: "1080p-medium",
            label: "1080p Medium",
            sublabel: "6 Mbps",
            resolution: "1080p",
            bitrateKbps: 6000,
          },
        ],
        activeId: "1080p",
        source: { height: 2160, bitrate_kbps: 25_000 },
        isTranscoding: false,
        error: null,
        onSelect: () => {},
      }),
    );

    expect(screen.getByRole("button", { name: "Quality" })).toHaveTextContent("1080p Medium");
    fireEvent.click(screen.getByRole("button", { name: "Quality" }));
    expect(screen.getByRole("menu")).toHaveClass("z-30");
    expect(screen.getByRole("menuitem", { name: /1080p Medium.*Selected/ })).toHaveAttribute(
      "aria-current",
      "true",
    );
  });

  it("lights no row for a fitting source under a fixed resolution unless a cap shows", () => {
    const options = [
      { id: "auto", label: "Auto", sublabel: "", resolution: "", bitrateKbps: 0 },
      {
        id: "1080p-medium",
        label: "1080p Medium",
        sublabel: "6 Mbps",
        resolution: "1080p",
        bitrateKbps: 6000,
      },
    ];
    const props = {
      options,
      activeId: "1080p",
      source: { height: 1080, bitrate_kbps: 20_000 },
      isTranscoding: false,
      error: null,
    };
    const { rerender } = render(
      createElement(QualityMenu, {
        ...props,
        deliveredRecipe: { bitrate_kbps: 20_000 },
        onSelect: () => {},
      }),
    );
    // The trigger names the saved resolution instead of lighting Auto.
    expect(screen.getByRole("button", { name: "Quality" })).toHaveTextContent("1080p");
    expect(screen.getByRole("button", { name: "Quality" })).not.toHaveTextContent("Auto");

    rerender(
      createElement(QualityMenu, {
        ...props,
        deliveredRecipe: { width: 1920, height: 1080, bitrate_kbps: 6000 },
        onSelect: () => {},
      }),
    );
    expect(screen.getByRole("button", { name: "Quality" })).toHaveTextContent("1080p Medium");
  });
});

describe("QualityMenu version format score", () => {
  it("shows the score badge when the server ranked the candidate", () => {
    renderVersionMenu({
      versions: [
        makeVersionInfo({ fileId: 1, label: "1080p H264", formatScore: 850 }),
        makeVersionInfo({ fileId: 2, label: "2160p HEVC" }),
      ],
    });

    expect(screen.getByText("★ 850")).toBeInTheDocument();
  });

  it("shows no badge for an unscored or zero-scored version", () => {
    renderVersionMenu({
      versions: [
        makeVersionInfo({ fileId: 1, label: "1080p H264" }),
        makeVersionInfo({ fileId: 2, label: "2160p HEVC", formatScore: 0 }),
      ],
    });

    expect(screen.queryByText(/★/)).not.toBeInTheDocument();
  });

  it("keeps a negative score visible so a demoted candidate is not mistaken for unscored", () => {
    renderVersionMenu({
      versions: [
        makeVersionInfo({ fileId: 1, label: "1080p H264", formatScore: -200 }),
        makeVersionInfo({ fileId: 2, label: "2160p HEVC" }),
      ],
    });

    expect(screen.getByText("★ -200")).toBeInTheDocument();
  });
});

describe("QualityMenu version row parity with the item picker", () => {
  it("renders audio and subtitle language badges", () => {
    renderVersionMenu({
      versions: [
        makeVersionInfo({
          fileId: 1,
          label: "2160p HEVC",
          audioLanguages: ["English", "French"],
          subtitleLanguages: ["German"],
        }),
        makeVersionInfo({ fileId: 2, label: "1080p H264" }),
      ],
    });

    const row = screen.getByRole("menuitem", { name: /2160p HEVC/ });
    expect(within(row).getByText("English")).toBeInTheDocument();
    expect(within(row).getByText("French")).toBeInTheDocument();
    expect(within(row).getByText("German")).toBeInTheDocument();
  });

  it("renders the shared release + size detail line", () => {
    renderVersionMenu({
      versions: [
        makeVersionInfo({
          fileId: 1,
          label: "2160p HEVC",
          detail: "Movie 2026 2160p WEB-DL · 47.1 GB",
        }),
        makeVersionInfo({ fileId: 2, label: "1080p H264" }),
      ],
    });

    expect(screen.getByText("Movie 2026 2160p WEB-DL · 47.1 GB")).toBeInTheDocument();
  });

  it("renders the ranking indicator and names the profile", () => {
    renderVersionMenu({
      versions: [
        makeVersionInfo({
          fileId: 1,
          label: "2160p HEVC",
          filePath: "virtual://movie/tt1?profile=4K%2BHDR&result=abc",
          profileLabel: "4K+HDR",
        }),
        makeVersionInfo({ fileId: 2, label: "1080p H264" }),
      ],
    });

    expect(screen.getByText(/Ranking: Default ranking/)).toBeInTheDocument();
    expect(screen.getByText(/4K\+HDR/)).toBeInTheDocument();
  });
});

describe("QualityMenu server virtual ranking", () => {
  it("renders the server payload's criteria and profile label when present", () => {
    renderVersionMenu({
      versions: [
        makeVersionInfo({
          fileId: 1,
          label: "2160p HEVC",
          virtualRanking: {
            profile_label: "4K HDR",
            source: "profile",
            criteria: [
              { attribute: "score", direction: "desc" },
              { attribute: "resolution", direction: "desc" },
            ],
          },
        }),
        makeVersionInfo({ fileId: 2, label: "1080p H264" }),
      ],
    });

    expect(screen.getByText(/Ranking: score ↓ · resolution ↓/)).toBeInTheDocument();
    expect(screen.getByText(/4K HDR/)).toBeInTheDocument();
  });

  it("falls back to the profile label and default order without the block", () => {
    renderVersionMenu({
      versions: [
        makeVersionInfo({
          fileId: 1,
          label: "2160p HEVC",
          filePath: "virtual://movie/tt1?profile=4K%2BHDR&result=abc",
          profileLabel: "4K+HDR",
        }),
        makeVersionInfo({ fileId: 2, label: "1080p H264" }),
      ],
    });

    expect(screen.getByText(/Ranking: Default ranking/)).toBeInTheDocument();
    expect(screen.getByText(/4K\+HDR/)).toBeInTheDocument();
    expect(screen.queryByText(/Ranking: score/)).not.toBeInTheDocument();
  });
});

describe("QualityMenu version sort preference", () => {
  function versionRowOrder() {
    return screen
      .getAllByRole("menuitem")
      .filter((row) => /HEVC|H264/.test(row.textContent ?? ""))
      .map((row) => (row.textContent?.includes("HEVC") ? "HEVC" : "H264"));
  }

  it("re-orders the version rows for the viewer without starting playback", () => {
    versionSortMock.criteria = [{ attribute: "size", direction: "desc" }];
    const onSwitchVersion = vi.fn();
    renderVersionMenu({
      onSwitchVersion,
      versions: [
        makeVersionInfo({ fileId: 1, label: "2160p HEVC", sortable: { fileSize: 100 } }),
        makeVersionInfo({ fileId: 2, label: "1080p H264", sortable: { fileSize: 300 } }),
      ],
    });

    expect(versionRowOrder()).toEqual(["H264", "HEVC"]);
    // Display-only: the reorder starts no playback and switches no version.
    expect(onSwitchVersion).not.toHaveBeenCalled();
  });

  it("keeps the server's incoming order with no override", () => {
    renderVersionMenu({
      versions: [
        makeVersionInfo({ fileId: 1, label: "2160p HEVC", sortable: { fileSize: 100 } }),
        makeVersionInfo({ fileId: 2, label: "1080p H264", sortable: { fileSize: 300 } }),
      ],
    });

    expect(versionRowOrder()).toEqual(["HEVC", "H264"]);
  });
});

describe("QualityMenu version list refresh", () => {
  it("renders Refresh List above and below the version rows", () => {
    renderVersionMenu({ onRefreshVersions: vi.fn().mockResolvedValue(undefined) });

    expect(screen.getByText("Version")).toBeInTheDocument();
    const rows = screen.getAllByRole("menuitem");
    expect(rows[0]).toHaveTextContent("Refresh List");
    // The header, then the top control, then the version rows.
    expect(rows[1]).toHaveTextContent("1080p H264");
    expect(rows[2]).toHaveTextContent("2160p HEVC");
    // The bottom control closes the version list, before the quality options.
    expect(rows[3]).toHaveTextContent("Refresh List");
    expect(rows[4]).toHaveTextContent("Original");
  });

  it("does not render any refresh row when no refresh handler is wired", () => {
    renderVersionMenu();

    expect(screen.queryByRole("menuitem", { name: /Refresh List/ })).not.toBeInTheDocument();
  });

  it("triggers exactly one refresh from the top control and locks both rows", async () => {
    let resolveRefresh: () => void = () => {};
    const onRefreshVersions = vi.fn(
      () =>
        new Promise<void>((resolve) => {
          resolveRefresh = resolve;
        }),
    );
    renderVersionMenu({ onRefreshVersions });

    const [topRefresh] = screen.getAllByRole("menuitem", { name: /Refresh List/ });
    fireEvent.click(topRefresh!);

    expect(onRefreshVersions).toHaveBeenCalledTimes(1);
    // Both controls read the one refreshing state, so both lock and show busy.
    const busyRows = screen.getAllByRole("menuitem", { name: /Refresh List/ });
    expect(busyRows).toHaveLength(2);
    for (const row of busyRows) {
      expect(row).toBeDisabled();
      expect(row).toHaveAttribute("aria-busy", "true");
    }

    // A second click while the request is in flight is ignored.
    fireEvent.click(busyRows[1]!);
    expect(onRefreshVersions).toHaveBeenCalledTimes(1);

    await act(async () => {
      resolveRefresh();
      await Promise.resolve();
    });
    for (const row of screen.getAllByRole("menuitem", { name: /Refresh List/ })) {
      expect(row).not.toBeDisabled();
    }
  });

  it("triggers exactly one refresh from the bottom control", async () => {
    const onRefreshVersions = vi.fn().mockResolvedValue(undefined);
    renderVersionMenu({ onRefreshVersions });

    const rows = screen.getAllByRole("menuitem", { name: /Refresh List/ });
    fireEvent.click(rows[1]!);

    expect(onRefreshVersions).toHaveBeenCalledTimes(1);
    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.getAllByRole("menuitem", { name: /Refresh List/ })).toHaveLength(2);
  });

  it("keeps both rows locked for the whole async job, not just acceptance", async () => {
    let finishJob: () => void = () => {};
    const onRefreshVersions = vi.fn(
      () =>
        new Promise<void>((resolve) => {
          finishJob = resolve;
        }),
    );
    renderVersionMenu({ onRefreshVersions });

    fireEvent.click(screen.getAllByRole("menuitem", { name: /Refresh List/ })[0]!);
    // Locked while the job runs; neither control is left enabled mid-job.
    for (const row of screen.getAllByRole("menuitem", { name: /Refresh List/ })) {
      expect(row).toBeDisabled();
    }

    await act(async () => {
      finishJob();
      await Promise.resolve();
    });
    for (const row of screen.getAllByRole("menuitem", { name: /Refresh List/ })) {
      expect(row).not.toBeDisabled();
    }
  });

  it("cancels from either control on a second press while running and unlocks both", async () => {
    let finishJob: (() => void) | undefined;
    const onRefreshVersions = vi.fn(
      () =>
        new Promise<void>((_resolve, reject) => {
          finishJob = () => reject(new Error("Job cancelled"));
        }),
    );
    const onCancelRefresh = vi.fn().mockResolvedValue(undefined);
    renderVersionMenu({ onRefreshVersions, onCancelRefresh });

    fireEvent.click(screen.getAllByRole("menuitem", { name: /Refresh List/ })[0]!);
    const running = screen.getAllByRole("menuitem", { name: /Cancel refresh/ });
    expect(running).toHaveLength(2);
    for (const row of running) {
      expect(row).not.toBeDisabled();
      expect(row).toHaveAttribute("aria-busy", "true");
    }

    // Pressing the bottom control cancels the job the top control started.
    fireEvent.click(running[1]!);
    expect(onCancelRefresh).toHaveBeenCalledTimes(1);

    await act(async () => {
      finishJob?.();
      await Promise.resolve();
    });
    expect(screen.queryByText(REFRESH_VERSIONS_ERROR)).not.toBeInTheDocument();
    for (const row of screen.getAllByRole("menuitem", { name: /Refresh List/ })) {
      expect(row).not.toBeDisabled();
    }
  });

  it("keeps the version rows, shows the message on both rows, and unlocks them after a failure", async () => {
    const onRefreshVersions = vi.fn().mockRejectedValue(new Error("provider unreachable"));
    renderVersionMenu({ onRefreshVersions });

    fireEvent.click(screen.getAllByRole("menuitem", { name: /Refresh List/ })[0]!);

    expect(await screen.findAllByText("provider unreachable")).toHaveLength(2);
    // The known candidates stay on screen next to the failure.
    expect(screen.getByRole("menuitem", { name: /1080p H264/ })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: /2160p HEVC/ })).toBeInTheDocument();
    for (const row of screen.getAllByRole("menuitem", { name: /Refresh List/ })) {
      expect(row).not.toBeDisabled();
    }
  });

  it("includes the top refresh row in the menu's arrow-key roving focus", () => {
    renderVersionMenu({ onRefreshVersions: vi.fn().mockResolvedValue(undefined) });

    const [topRefresh] = screen.getAllByRole("menuitem", { name: /Refresh List/ });
    const firstVersionRow = screen.getByRole("menuitem", { name: /1080p H264/ });

    // Arrow Up from the first version row reaches the top refresh control.
    firstVersionRow.focus();
    expect(firstVersionRow).toHaveFocus();
    fireEvent.keyDown(screen.getByRole("menu"), { key: "ArrowUp" });
    expect(topRefresh).toHaveFocus();
  });
});

describe("QualityMenu indexer releases", () => {
  const release = (overrides: Partial<PlayerIndexerRelease> = {}): PlayerIndexerRelease => ({
    release_id: "rel-1",
    title: "Movie.2026.2160p.WEB-DL.DDP5.1",
    download_state: "not_downloaded",
    ...overrides,
  });

  it("renders indexer rows below the playable versions with a Not downloaded badge", async () => {
    renderVersionMenu({
      contentId: "content-1",
      indexerReleases: [
        release({ resolution: "2160p", codec_video: "hevc", size_bytes: 5_000_000_000 }),
      ],
    });

    expect(await screen.findByText("Not downloaded")).toBeInTheDocument();
    expect(screen.getByText(/2160p · HEVC/)).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: /Request Movie/ })).toBeInTheDocument();

    // The indexer row comes after the playable version rows.
    const items = screen.getAllByRole("menuitem");
    const playable = items.findIndex((row) => /1080p H264|2160p HEVC/.test(row.textContent ?? ""));
    const indexer = items.findIndex((row) => /Not downloaded/.test(row.textContent ?? ""));
    expect(indexer).toBeGreaterThan(playable);
  });

  it("posts the Request to the release endpoint and flips the row to Requested", async () => {
    let resolveRequest: (value: { release_id: string; state: "queued" }) => void = () => {};
    requestReleaseMock.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveRequest = resolve;
        }),
    );
    renderVersionMenu({ contentId: "content-1", indexerReleases: [release()] });

    fireEvent.click(await screen.findByRole("menuitem", { name: /Request Movie/ }));
    expect(requestReleaseMock).toHaveBeenCalledWith("content-1", "rel-1");
    expect(screen.getByRole("menuitem", { name: /Request Movie/ })).toBeDisabled();

    await act(async () => {
      resolveRequest({ release_id: "rel-1", state: "queued" });
      await Promise.resolve();
    });

    const row = screen.getByRole("menuitem", { name: /Requested Movie/ });
    expect(row).toBeDisabled();
    expect(within(row).getByText("Requested")).toBeInTheDocument();
  });

  it("keeps the row retryable when the request fails", async () => {
    requestReleaseMock.mockRejectedValueOnce(new Error("network"));
    renderVersionMenu({ contentId: "content-1", indexerReleases: [release()] });

    fireEvent.click(await screen.findByRole("menuitem", { name: /Request Movie/ }));

    expect(await screen.findByText(REQUEST_RELEASE_ERROR)).toBeInTheDocument();
    const retry = screen.getByRole("menuitem", { name: /Request Movie/ });
    expect(retry).not.toBeDisabled();
    expect(retry).toHaveTextContent("Retry");
  });

  it("renders nothing new when no indexer releases are present", async () => {
    renderVersionMenu({ indexerReleases: [] });

    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.queryByText("Not downloaded")).not.toBeInTheDocument();
  });

  it("includes indexer rows in the menu's arrow-key roving focus", async () => {
    renderVersionMenu({
      contentId: "content-1",
      indexerReleases: [release()],
    });
    const indexerRow = await screen.findByRole("menuitem", { name: /Request Movie/ });
    // The second version row is the last registered item before the indexer
    // rows; Arrow Down from it steps onto the first indexer row.
    const lastVersionRow = screen.getByRole("menuitem", { name: /2160p HEVC/ });

    lastVersionRow.focus();
    expect(lastVersionRow).toHaveFocus();
    fireEvent.keyDown(screen.getByRole("menu"), { key: "ArrowDown" });
    expect(indexerRow).toHaveFocus();

    // Arrow Down again leaves the indexer row for the next registered item.
    fireEvent.keyDown(screen.getByRole("menu"), { key: "ArrowDown" });
    expect(indexerRow).not.toHaveFocus();
  });

  it("hides the indexer UI when the server capability is off", async () => {
    capabilityMock.mockResolvedValue({ state: "available", indexer_request: false });
    renderVersionMenu({ contentId: "content-1", indexerReleases: [release()] });

    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(screen.queryByText("Not downloaded")).not.toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: /Request Movie/ })).not.toBeInTheDocument();
  });
});

describe("QualityMenu automatic version fallback", () => {
  it("renders no Auto row when fallback cannot be toggled", () => {
    renderVersionMenu();
    expect(screen.queryByRole("menuitemradio", { name: /Auto/ })).not.toBeInTheDocument();
  });

  it("arms automatic fallback from the Auto row", () => {
    const onSelectAutoVersion = vi.fn();
    renderVersionMenu({ onSelectAutoVersion, autoFallback: false });

    const autoRow = screen.getByRole("menuitemradio", { name: /Auto/ });
    expect(autoRow).toHaveAttribute("aria-checked", "false");
    fireEvent.click(autoRow);
    expect(onSelectAutoVersion).toHaveBeenCalledTimes(1);
  });

  it("marks Auto checked when armed and keeps the version rows selectable", () => {
    renderVersionMenu({ onSelectAutoVersion: () => {}, autoFallback: true });

    expect(screen.getByRole("menuitemradio", { name: /Auto/ })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    expect(screen.getByRole("menuitem", { name: /1080p H264/ })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: /2160p HEVC/ })).toBeInTheDocument();
  });
});

const rung720 = {
  id: "720p",
  label: "720p",
  sublabel: "3 Mbps",
  resolution: "720p",
  bitrateKbps: 3000,
};
const auto = {
  id: "auto",
  label: "Auto",
  sublabel: "",
  resolution: "",
  bitrateKbps: 0,
};
const versions = [
  makeVersionInfo({ isCurrentSource: true }),
  makeVersionInfo({ fileId: 2, label: "1080p H264" }),
];
function menuProps(
  overrides: Partial<ComponentProps<typeof QualityMenu>> = {},
): ComponentProps<typeof QualityMenu> {
  return {
    options: [rung720],
    activeId: "auto",
    isTranscoding: false,
    error: null,
    onSelect: vi.fn(),
    versions,
    onSwitchVersion: vi.fn(),
    ...overrides,
  };
}

describe("available playback choices", () => {
  it.each([{ options: [] }, { options: [rung720] }])(
    "keeps version switching without a quality choice (%j)",
    ({ options }) => {
      const props = menuProps({ options });
      render(createElement(QualityMenu, props));
      fireEvent.click(screen.getByRole("button", { name: "Version" }));
      expect(screen.queryByText("Quality")).not.toBeInTheDocument();
      expect(screen.queryByRole("menuitem", { name: /720p/ })).not.toBeInTheDocument();
      fireEvent.click(screen.getByRole("menuitem", { name: /1080p H264/ }));
      expect(props.onSwitchVersion).toHaveBeenCalledWith(2);
      expect(props.onSelect).not.toHaveBeenCalled();
    },
  );

  it.each([
    { versions: [makeVersionInfo()] },
    { versionLocked: true },
    { onSwitchVersion: undefined },
    { options: [], versions: [] },
  ])("hides the trigger when neither choice is actionable (%j)", (overrides) => {
    render(createElement(QualityMenu, menuProps(overrides)));
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });

  it("shows both sections when versions and qualities are available", () => {
    render(createElement(QualityMenu, menuProps({ options: [auto, rung720] })));
    fireEvent.click(screen.getByRole("button", { name: "Quality" }));
    expect(screen.getByText("Version")).toBeInTheDocument();
    expect(screen.getByText("Quality")).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: /Auto.*Selected/ })).toHaveAttribute(
      "aria-current",
      "true",
    );
  });

  it("preserves Auto when playback changes from one quality to multiple qualities", () => {
    const props = menuProps();
    const { rerender } = render(createElement(QualityMenu, props));
    expect(screen.getByRole("button", { name: "Version" })).toHaveTextContent("Version");
    rerender(createElement(QualityMenu, { ...props, options: [auto, rung720] }));
    fireEvent.click(screen.getByRole("button", { name: "Quality" }));
    expect(screen.getByRole("menuitem", { name: /Auto.*Selected/ })).toBeInTheDocument();
    expect(props.onSelect).not.toHaveBeenCalled();
  });
});

describe("playback speed", () => {
  it("keeps the menu for speed alone and picks a speed without closing it", () => {
    const onPlaybackSpeedChange = vi.fn();
    render(
      createElement(
        QualityMenu,
        menuProps({ options: [rung720], versions: undefined, onPlaybackSpeedChange }),
      ),
    );
    const trigger = screen.getByRole("button", { name: "Playback settings" });

    fireEvent.click(trigger);
    expect(screen.getByRole("group", { name: "Speed" })).toBeInTheDocument();
    expect(screen.getAllByRole("menuitemradio")).toHaveLength(9);
    expect(screen.getByRole("menuitemradio", { name: "1×" })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    fireEvent.click(screen.getByRole("menuitemradio", { name: "1.5×" }));
    expect(onPlaybackSpeedChange).toHaveBeenCalledExactlyOnceWith(1.5);
    // The menu stays open so the viewer sees the speed take effect.
    expect(screen.getByRole("menu")).toBeInTheDocument();
  });

  it("moves between speeds with the left and right arrow keys", () => {
    render(createElement(QualityMenu, menuProps({ onPlaybackSpeedChange: vi.fn() })));
    fireEvent.click(screen.getByRole("button", { name: "Playback settings" }));
    const half = screen.getByRole("menuitemradio", { name: "0.5×" });
    half.focus();
    fireEvent.keyDown(half, { key: "ArrowRight" });
    expect(screen.getByRole("menuitemradio", { name: "0.75×" })).toHaveFocus();
    fireEvent.keyDown(document.activeElement!, { key: "ArrowLeft" });
    expect(half).toHaveFocus();
  });

  it("keeps focus in the menu when the track is pressed between stops", () => {
    render(createElement(QualityMenu, menuProps({ onPlaybackSpeedChange: vi.fn() })));
    fireEvent.click(screen.getByRole("button", { name: "Playback settings" }));
    const track = screen.getByRole("menuitemradio", { name: "1×" }).parentElement!;
    expect(fireEvent.pointerDown(track, { button: 0 })).toBe(false);
    expect(
      fireEvent.pointerDown(screen.getByRole("menuitemradio", { name: "2×" }), { button: 0 }),
    ).toBe(true);
  });

  it("badges the gear with a speed other than 1x", () => {
    const props = menuProps({
      options: [auto, rung720],
      activeId: "720p",
      playbackSpeed: 1,
      onPlaybackSpeedChange: vi.fn(),
    });
    const { rerender } = render(createElement(QualityMenu, props));
    expect(screen.getByRole("button", { name: "Playback settings" })).toHaveTextContent(/^720p$/);

    rerender(createElement(QualityMenu, { ...props, playbackSpeed: 1.5 }));
    const trigger = screen.getByRole("button", { name: "Playback settings, 1.5×" });
    expect(trigger.querySelector(".player-quality-label")).toHaveTextContent(/^720p$/);
    expect(trigger.querySelector(".player-speed-badge")).toHaveTextContent(/^1\.5×$/);
  });
});

it("shows original provider text and optionally adds quality and languages", async () => {
  localStorage.clear();
  const original = "⚡ Provider\n🎬 Movie.WEB-DL\n🇪🇸 Spanish";
  renderVersionMenu({
    versions: [
      makeVersionInfo({
        providerText: original,
        label: "1080p · WEB-DL",
        audioLanguages: ["Spanish"],
        unavailable: true,
      }),
      makeVersionInfo({ fileId: 2, label: "Other version" }),
    ],
  });
  const text = await screen.findByText(
    (_, element) => element?.childElementCount === 0 && element.textContent === original,
  );
  expect(text).toHaveClass("whitespace-pre-wrap");
  expect(screen.queryByText("Spanish")).not.toBeInTheDocument();
  expect(screen.getByText("Will retry on play")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("switch", { name: /Enrich/ }));
  expect(screen.getByText("1080p · WEB-DL")).toBeInTheDocument();
  expect(screen.getByText("Spanish")).toBeInTheDocument();
  expect(text).toBeInTheDocument();
});

it("keeps provider enrichment in the keyboard order before refresh and Auto", () => {
  localStorage.clear();
  renderVersionMenu({
    versions: [
      makeVersionInfo({ providerText: "Provider result" }),
      makeVersionInfo({ fileId: 2 }),
    ],
    onRefreshVersions: vi.fn().mockResolvedValue(undefined),
    onSelectAutoVersion: vi.fn(),
  });
  const toggle = screen.getByRole("switch", { name: /Enrich/ });
  toggle.focus();
  fireEvent.keyDown(toggle, { key: "ArrowDown" });
  expect(screen.getAllByRole("menuitem", { name: /Refresh List/ })[0]).toHaveFocus();
  fireEvent.keyDown(document.activeElement!, { key: "ArrowDown" });
  expect(screen.getByRole("menuitemradio", { name: /Auto/ })).toHaveFocus();
});
