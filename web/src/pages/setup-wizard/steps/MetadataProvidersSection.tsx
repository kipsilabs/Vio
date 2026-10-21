import { useEffect, useRef, useState } from "react";

import { useQueryClient } from "@tanstack/react-query";

import { Button } from "@/components/ui/button";
import { useAdminPlugins, useInstallPlugin } from "@/hooks/queries/admin/plugins";
import { adminKeys } from "@/hooks/queries/keys";

import { StepSection } from "../StepFrame";

// First-party metadata providers installed automatically during onboarding so
// new libraries match against TMDB and TVDB without a trip to Admin › Plugins.
// Installing from the catalog appends each provider to the library chains, and
// reinstalling an existing plugin_id replaces rather than duplicates, so a
// repeated run converges instead of stacking installs.
const DEFAULT_METADATA_PLUGINS = [
  {
    plugin_id: "silo.tmdb",
    label: "TMDB Metadata",
    blurb: "Movies, series, seasons, and episodes from The Movie Database.",
  },
  {
    plugin_id: "silo.tvdb",
    label: "TVDB Metadata",
    blurb: "Series, seasons, and episodes from TheTVDB.",
  },
];

type PluginState = "pending" | "installing" | "installed" | "failed" | "unavailable";

export function MetadataProvidersSection() {
  const { catalog, installations, isLoading } = useAdminPlugins();
  const install = useInstallPlugin();
  const queryClient = useQueryClient();
  // Auto-install fires once per mount. The install endpoint replaces an
  // existing plugin_id rather than duplicating it, so even a repeated run
  // (StrictMode, remount, retry) converges on one installation per provider.
  const startedForAttempt = useRef(-1);
  const [attempt, setAttempt] = useState(0);
  const [failed, setFailed] = useState<Record<string, boolean>>({});
  const [installing, setInstalling] = useState<Record<string, boolean>>({});

  const installedIds = new Set(installations.map((i) => i.plugin_id));
  const catalogById = new Map(catalog.map((entry) => [entry.plugin_id, entry]));

  async function installOne(plugin_id: string) {
    const entry = catalogById.get(plugin_id);
    if (!entry) return;
    setInstalling((prev) => ({ ...prev, [plugin_id]: true }));
    setFailed((prev) => ({ ...prev, [plugin_id]: false }));
    try {
      await install.mutateAsync({
        repository_id: entry.repository_id,
        plugin_id: entry.plugin_id,
        version: entry.version,
      });
    } catch {
      // useInstallPlugin already toasts; the row keeps a Retry action.
      setFailed((prev) => ({ ...prev, [plugin_id]: true }));
    } finally {
      setInstalling((prev) => ({ ...prev, [plugin_id]: false }));
    }
  }

  useEffect(() => {
    if (isLoading || startedForAttempt.current === attempt) return;
    startedForAttempt.current = attempt;
    for (const { plugin_id } of DEFAULT_METADATA_PLUGINS) {
      if (installedIds.has(plugin_id)) continue;
      if (!catalogById.has(plugin_id)) continue;
      void installOne(plugin_id);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- run once per attempt; data arrives through the shared plugin queries.
  }, [isLoading, attempt]);

  // Re-checks the catalog and installations (e.g. after an offline failure or
  // a catalog without these entries) and re-runs the auto-install pass.
  function recheck() {
    startedForAttempt.current = -1;
    setFailed({});
    setInstalling({});
    void queryClient.invalidateQueries({ queryKey: adminKeys.pluginCatalog() });
    void queryClient.invalidateQueries({ queryKey: adminKeys.pluginInstallations() });
    setAttempt((n) => n + 1);
  }

  function stateOf(plugin_id: string): PluginState {
    if (installedIds.has(plugin_id)) return "installed";
    if (installing[plugin_id]) return "installing";
    if (failed[plugin_id]) return "failed";
    if (isLoading) return "pending";
    if (!catalogById.has(plugin_id)) return "unavailable";
    return "pending";
  }

  return (
    <StepSection
      title="Metadata providers"
      caption="TMDB and TVDB identify your movies and shows. Vio installs them automatically; a failure here never blocks setup and can be retried from Admin › Plugins."
    >
      <ul className="space-y-2">
        {DEFAULT_METADATA_PLUGINS.map(({ plugin_id, label, blurb }) => {
          const state = stateOf(plugin_id);
          return (
            <li
              key={plugin_id}
              className="border-border flex items-center justify-between gap-3 rounded-lg border px-4 py-3"
            >
              <div>
                <p className="text-sm font-medium">{label}</p>
                <p className="text-muted-foreground text-xs">{blurb}</p>
              </div>
              {state === "installed" ? (
                <span className="text-xs font-medium text-green-600 dark:text-green-400">
                  Installed
                </span>
              ) : state === "installing" || (state === "pending" && !isLoading) ? (
                <span className="text-muted-foreground text-xs">
                  {state === "installing" ? "Installing…" : "Starting…"}
                </span>
              ) : state === "failed" ? (
                <span className="flex items-center gap-2">
                  <span className="text-xs text-amber-600 dark:text-amber-400">Install failed</span>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    onClick={() => installOne(plugin_id)}
                  >
                    Retry
                  </Button>
                </span>
              ) : state === "unavailable" ? (
                <span className="flex items-center gap-2">
                  <span className="text-muted-foreground text-xs">Not in plugin catalog</span>
                  <Button type="button" variant="outline" size="sm" onClick={recheck}>
                    Retry
                  </Button>
                </span>
              ) : (
                <span className="text-muted-foreground text-xs">Checking…</span>
              )}
            </li>
          );
        })}
      </ul>
    </StepSection>
  );
}
