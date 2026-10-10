/* eslint-disable react-refresh/only-export-components */
import { useMemo, useState } from "react";
import { Link } from "react-router";
import { AlertTriangle, ArrowRight, Loader2, Sparkles } from "lucide-react";

import {
  ConnectionCheckAction,
  useConnectionCheck,
} from "@/components/admin/ConnectionCheckAction";
import { SettingsPageHeader } from "@/components/settings/SettingsPageHeader";
import { SecretField } from "@/components/settings/SecretField";
import { VioScoringProfilesCard } from "@/components/streaming/VioScoringProfilesCard";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { useAdminLibraries, useCreateLibrary } from "@/hooks/queries/admin/libraries";
import { useRestartKeys } from "@/hooks/useRestartKeys";
import { useSettingsForm } from "@/hooks/useSettingsForm";

import { FieldGroup } from "./FieldGroup";
import { SaveBar } from "@/components/SaveBar";
import { SettingField, SettingFieldStatus } from "./SettingField";

const PROVIDER_KEYS = [
  "virtual_library.enabled",
  "virtual_library.manifest_url",
  "virtual_library.tmdb_api_key",
  "virtual_library.cache_ttl_minutes",
  "virtual_library.candidate_store_hours",
];

const NETWORK_KEYS = [
  "virtual_library.allow_insecure_http",
  "virtual_library.allow_private_streams",
];

const LIBRARY_KEYS = [
  "virtual_library.movie_library_id",
  "virtual_library.series_library_id",
  "virtual_library.monitor_file",
  "virtual_library.schedule_refresh_minutes",
];

const QUALITY_KEYS = [
  "virtual_library.enable_quality_profiles",
  "virtual_library.quality_preset",
  "virtual_library.custom_format_preset",
  "virtual_library.single_stream_with_failover",
  "virtual_library.fallback_to_any_stream",
  "virtual_library.quality_profiles",
  "virtual_library.custom_formats",
];

const AUTOMATION_KEYS = [
  "virtual_library.indexer_rss_url",
  "virtual_library.indexer_api_key",
  "virtual_library.indexer_rss_check_minutes",
  "virtual_library.indexer_search_timeout_seconds",
  "virtual_library.altmount_url",
  "virtual_library.altmount_api_key",
  "virtual_library.altmount_check_minutes",
];

const ALL_KEYS = [
  ...PROVIDER_KEYS,
  ...NETWORK_KEYS,
  ...LIBRARY_KEYS,
  ...QUALITY_KEYS,
  ...AUTOMATION_KEYS,
];

/** Feedback shown under the Prowlarr URL field. */
export interface ProwlarrURLFeedback {
  tone: "ok" | "warn";
  message: string;
}

const PROWLARR_BASE_URL_ONLY = "Enter the base URL only — no indexer path or query string";
const PROWLARR_FULL_BASE_URL = "Enter the full base URL, including http:// or https://";

/**
 * Client-side mirror of the server's validateProwlarrBaseURL. The setting takes
 * Prowlarr's server base URL; an indexer-scoped path or a query string makes the
 * client append "/api/v1/search" onto a newznab route, which Prowlarr answers
 * with 400. Returns null for an empty value so the feedback row is hidden until
 * something is typed.
 */
export function prowlarrURLFeedback(raw: string): ProwlarrURLFeedback | null {
  const trimmed = raw.trim();
  if (trimmed === "") {
    return null;
  }
  let parsed: URL;
  try {
    parsed = new URL(trimmed);
  } catch {
    return { tone: "warn", message: PROWLARR_FULL_BASE_URL };
  }
  if ((parsed.protocol !== "http:" && parsed.protocol !== "https:") || parsed.host === "") {
    return { tone: "warn", message: PROWLARR_FULL_BASE_URL };
  }
  if (parsed.search !== "" || parsed.hash !== "" || hasProwlarrIndexerPath(parsed.pathname)) {
    return { tone: "warn", message: PROWLARR_BASE_URL_ONLY };
  }
  return { tone: "ok", message: "Base URL looks good" };
}

/**
 * Mirrors the server's hasIndexerPath: a trailing numeric segment is Prowlarr's
 * indexer id, and "api", "newznab" and "torznab" segments are the search proxy
 * routes the client would otherwise duplicate. A reverse-proxy subpath such as
 * /prowlarr is a legitimate base and is not flagged.
 */
function hasProwlarrIndexerPath(pathname: string): boolean {
  const segments = pathname.replace(/^\/+|\/+$/g, "").split("/");
  for (let i = 0; i < segments.length; i += 1) {
    const segment = (segments[i] ?? "").toLowerCase();
    if (segment === "") continue;
    if (segment === "api" || segment === "newznab" || segment === "torznab") {
      return true;
    }
    if (i === segments.length - 1 && /^\d+$/.test(segment)) {
      return true;
    }
  }
  return false;
}

export default function StreamingSettings() {
  const form = useSettingsForm({ keys: useMemo(() => ALL_KEYS, []) });
  const restartKeys = useRestartKeys();
  const { data: libraries } = useAdminLibraries();
  const createLibrary = useCreateLibrary();
  const [creatingLibs, setCreatingLibs] = useState(false);

  const providerCheck = useConnectionCheck("virtual_library", form, [
    ...PROVIDER_KEYS,
    "virtual_library.allow_insecure_http",
  ]);

  const anyDirty = (keys: string[]) => keys.some((key) => form.isDirty(key));

  // Inline base-URL feedback for the Prowlarr field: green when the value can
  // take an appended "/api/v1/search", amber with the reason when it cannot.
  const prowlarrFeedback = prowlarrURLFeedback(form.getValue("virtual_library.indexer_rss_url"));

  const movieOptions = useMemo(() => {
    if (!libraries || libraries.length === 0) return [];
    return libraries
      .filter((l) => l.type === "movies" || l.type === "mixed")
      .map((l) => ({ value: String(l.id), label: `${l.name} (ID: ${l.id})` }));
  }, [libraries]);

  const seriesOptions = useMemo(() => {
    if (!libraries || libraries.length === 0) return [];
    return libraries
      .filter((l) => l.type === "series" || l.type === "mixed")
      .map((l) => ({ value: String(l.id), label: `${l.name} (ID: ${l.id})` }));
  }, [libraries]);

  const currentMovieID = form.getValue("virtual_library.movie_library_id");
  const movieSelectOptions = useMemo(() => {
    const opts = [...movieOptions];
    if (currentMovieID && !opts.some((o) => o.value === currentMovieID)) {
      opts.unshift({ value: currentMovieID, label: `Library #${currentMovieID}` });
    }
    return opts;
  }, [movieOptions, currentMovieID]);

  const currentSeriesID = form.getValue("virtual_library.series_library_id");
  const seriesSelectOptions = useMemo(() => {
    const opts = [...seriesOptions];
    if (currentSeriesID && !opts.some((o) => o.value === currentSeriesID)) {
      opts.unshift({ value: currentSeriesID, label: `Library #${currentSeriesID}` });
    }
    return opts;
  }, [seriesOptions, currentSeriesID]);

  const handleCreateVirtualLibraries = async () => {
    setCreatingLibs(true);
    try {
      const hasMovies = libraries?.some(
        (l) =>
          (l.type === "movies" || l.type === "mixed") &&
          l.paths.some((p) => p.startsWith("virtual://")),
      );
      const hasSeries = libraries?.some(
        (l) =>
          (l.type === "series" || l.type === "mixed") &&
          l.paths.some((p) => p.startsWith("virtual://")),
      );

      if (!hasMovies) {
        const mov = await createLibrary.mutateAsync({
          name: "Virtual Movies",
          type: "movies",
          paths: ["virtual://movies"],
        });
        form.setValue("virtual_library.movie_library_id", String(mov.id));
      }
      if (!hasSeries) {
        const ser = await createLibrary.mutateAsync({
          name: "Virtual Series",
          type: "series",
          paths: ["virtual://series"],
        });
        form.setValue("virtual_library.series_library_id", String(ser.id));
      }
    } finally {
      setCreatingLibs(false);
    }
  };

  if (form.isLoading)
    return (
      <div className="space-y-6" role="status" aria-label="Loading settings">
        <Skeleton className="h-8 w-40" />
        <div className="space-y-4">
          <Skeleton className="h-10 w-full" />
          <Skeleton className="h-10 w-full" />
        </div>
        <span className="sr-only">Loading settings</span>
      </div>
    );

  return (
    <div className="flex h-full flex-col">
      <SettingsPageHeader title="Streaming" className="mb-8" />

      <div className="flex-1 space-y-6">
        <div className="border-border/70 bg-card/60 flex items-center justify-between rounded-xl border p-4 backdrop-blur-sm">
          <div className="space-y-0.5">
            <div className="text-sm font-semibold">Virtual Release Desk</div>
            <p className="text-muted-foreground text-xs">
              Inspect zero-storage monitored items, candidate stream health, failed deliveries, and
              recent releases.
            </p>
          </div>
          <Button variant="outline" size="sm" asChild className="shrink-0">
            <Link to="/admin/virtual-library">
              Open Release Desk
              <ArrowRight className="ml-1.5 h-3.5 w-3.5" />
            </Link>
          </Button>
        </div>

        <FieldGroup label="Stremio provider" dirty={anyDirty(PROVIDER_KEYS)}>
          <SettingField
            label="Enable streaming"
            type="toggle"
            description="Fetch stream candidates from the configured Stremio provider."
            value={form.getValue("virtual_library.enabled") || "true"}
            onChange={(v) => form.setValue("virtual_library.enabled", v)}
            restartRequired={restartKeys.has("virtual_library.enabled")}
          />
          <SettingField
            label="Manifest URL"
            description="Tokenized provider URL ending in /manifest.json. HTTPS required; allow local HTTP only for private hosts."
            hint="https://…/manifest.json"
            value={form.getValue("virtual_library.manifest_url")}
            onChange={(v) => form.setValue("virtual_library.manifest_url", v)}
            restartRequired={restartKeys.has("virtual_library.manifest_url")}
          />
          <SettingField
            label="TMDB API key"
            description="Optional: resolves TMDB IDs to provider IDs for broader coverage."
            value={form.getValue("virtual_library.tmdb_api_key")}
            onChange={(v) => form.setValue("virtual_library.tmdb_api_key", v)}
            restartRequired={restartKeys.has("virtual_library.tmdb_api_key")}
          />
          <SettingField
            label="Cache TTL (minutes)"
            type="number"
            description="How long provider candidates are reused before re-fetching. 1–10080."
            value={form.getValue("virtual_library.cache_ttl_minutes")}
            onChange={(v) => form.setValue("virtual_library.cache_ttl_minutes", v)}
            restartRequired={restartKeys.has("virtual_library.cache_ttl_minutes")}
          />
          <SettingField
            label="Candidate trust window (hours)"
            type="number"
            description="How long a stored candidate stays trusted for replay after the provider's list drops it. 0 disables; 0–8760."
            value={form.getValue("virtual_library.candidate_store_hours")}
            onChange={(v) => form.setValue("virtual_library.candidate_store_hours", v)}
            restartRequired={restartKeys.has("virtual_library.candidate_store_hours")}
          />
          <ConnectionCheckAction
            onClick={providerCheck.run}
            result={providerCheck.result}
            isPending={providerCheck.isPending}
            disabled={!form.getValue("virtual_library.manifest_url")}
          />
        </FieldGroup>

        <FieldGroup label="Network access" dirty={anyDirty(NETWORK_KEYS)}>
          <SettingField
            label="Allow HTTP for local manifests"
            type="toggle"
            description="Permits http:// manifest URLs on private/local networks only. HTTPS remains required for public hosts."
            value={form.getValue("virtual_library.allow_insecure_http") || "false"}
            onChange={(v) => form.setValue("virtual_library.allow_insecure_http", v)}
            restartRequired={restartKeys.has("virtual_library.allow_insecure_http")}
          />
          <SettingField
            label="Allow private network streams"
            type="toggle"
            description="Lets virtual-stream playback contact non-public addresses (localhost, LAN, link-local)."
            value={form.getValue("virtual_library.allow_private_streams") || "false"}
            onChange={(v) => form.setValue("virtual_library.allow_private_streams", v)}
            restartRequired={restartKeys.has("virtual_library.allow_private_streams")}
          />
          {form.getValue("virtual_library.allow_private_streams") === "true" && (
            <div className="settings-field-note my-3 flex items-start gap-3 rounded-xl border border-amber-500/20 bg-amber-500/5 p-4">
              <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-amber-500" />
              <div className="text-[13px] leading-relaxed">
                <p className="font-medium text-amber-500">Private network access</p>
                <p className="text-muted-foreground mt-1">
                  When enabled, virtual-stream playback may contact non-public addresses including
                  localhost and link-local services. A compromised provider can cause Silo to
                  request internal services. TLS certificate checks remain enabled.
                </p>
              </div>
            </div>
          )}
        </FieldGroup>

        <FieldGroup label="Virtual libraries" dirty={anyDirty(LIBRARY_KEYS)}>
          <SettingField
            label="Movies library"
            settingKey="virtual_library.movie_library_id"
            type={movieSelectOptions.length > 0 ? "select" : "number"}
            options={movieSelectOptions.length > 0 ? movieSelectOptions : undefined}
            description="Virtual movies library (virtual://movies) holding provider titles."
            value={form.getValue("virtual_library.movie_library_id")}
            onChange={(v) => form.setValue("virtual_library.movie_library_id", v)}
            restartRequired={restartKeys.has("virtual_library.movie_library_id")}
          />
          <SettingField
            label="Series library"
            settingKey="virtual_library.series_library_id"
            type={seriesSelectOptions.length > 0 ? "select" : "number"}
            options={seriesSelectOptions.length > 0 ? seriesSelectOptions : undefined}
            description="Virtual series library (virtual://series) holding provider episodes."
            value={form.getValue("virtual_library.series_library_id")}
            onChange={(v) => form.setValue("virtual_library.series_library_id", v)}
            restartRequired={restartKeys.has("virtual_library.series_library_id")}
          />
          <div className="border-border/60 flex flex-col gap-2 border-b pt-1 pb-3 sm:flex-row sm:items-center sm:justify-between">
            <div>
              <p className="text-xs font-medium">Auto-create zero-storage libraries</p>
              <p className="text-muted-foreground text-xs">
                Quickly create Virtual Movies (virtual://movies) and Virtual Series
                (virtual://series) if you haven't set them up yet.
              </p>
            </div>
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={creatingLibs || createLibrary.isPending}
              onClick={handleCreateVirtualLibraries}
              className="shrink-0"
            >
              {creatingLibs || createLibrary.isPending ? (
                <>
                  <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" />
                  Creating...
                </>
              ) : (
                <>
                  <Sparkles className="mr-1.5 h-3.5 w-3.5" />
                  Create Virtual Libraries
                </>
              )}
            </Button>
          </div>
          <SettingField
            label="Monitor file"
            description="Filename tracking monitored titles inside each virtual library."
            value={form.getValue("virtual_library.monitor_file")}
            onChange={(v) => form.setValue("virtual_library.monitor_file", v)}
            restartRequired={restartKeys.has("virtual_library.monitor_file")}
          />
          <SettingField
            label="Schedule refresh (minutes)"
            type="number"
            description="How often monitored titles re-check the provider. 30–10080."
            value={form.getValue("virtual_library.schedule_refresh_minutes")}
            onChange={(v) => form.setValue("virtual_library.schedule_refresh_minutes", v)}
            restartRequired={restartKeys.has("virtual_library.schedule_refresh_minutes")}
          />
        </FieldGroup>

        <FieldGroup
          label="Automation & Indexers (Prowlarr & AltMount)"
          dirty={anyDirty(AUTOMATION_KEYS)}
        >
          <SettingField
            label="Prowlarr URL"
            description="Optional: Prowlarr server base URL for automated release discovery. Enter the base URL only — no indexer path or query string; the API key is set below."
            hint="http://prowlarr:9696"
            value={form.getValue("virtual_library.indexer_rss_url")}
            onChange={(v) => form.setValue("virtual_library.indexer_rss_url", v)}
            restartRequired={restartKeys.has("virtual_library.indexer_rss_url")}
            status={
              prowlarrFeedback ? (
                <SettingFieldStatus tone={prowlarrFeedback.tone}>
                  {prowlarrFeedback.message}
                </SettingFieldStatus>
              ) : undefined
            }
          />
          <SecretField
            label="Prowlarr API key"
            hint="API key for authenticated Prowlarr indexer queries."
            value={form.getValue("virtual_library.indexer_api_key")}
            configured={form.sensitiveConfigured.includes("virtual_library.indexer_api_key")}
            onChange={(v) => form.setValue("virtual_library.indexer_api_key", v)}
            onKeep={() => form.resetValue("virtual_library.indexer_api_key")}
            // Nothing else on this page can empty the stored key, and a
            // Prowlarr instance without auth needs it empty.
            onClear={() => form.setValue("virtual_library.indexer_api_key", "")}
            cleared={form.isClearStaged("virtual_library.indexer_api_key")}
            restartRequired={restartKeys.has("virtual_library.indexer_api_key")}
          />
          <SettingField
            label="Prowlarr check interval (minutes)"
            type="number"
            description="How often to poll Prowlarr for new releases, in minutes. 1–10080."
            value={form.getValue("virtual_library.indexer_rss_check_minutes") || "15"}
            onChange={(v) => form.setValue("virtual_library.indexer_rss_check_minutes", v)}
            restartRequired={restartKeys.has("virtual_library.indexer_rss_check_minutes")}
          />
          <SettingField
            label="Prowlarr search timeout (seconds)"
            type="number"
            description="How long to wait for Prowlarr to aggregate a search before giving up. Raise it if Prowlarr searches many slow indexers. 5–900."
            value={form.getValue("virtual_library.indexer_search_timeout_seconds") || "20"}
            onChange={(v) => form.setValue("virtual_library.indexer_search_timeout_seconds", v)}
            restartRequired={restartKeys.has("virtual_library.indexer_search_timeout_seconds")}
          />
          <SettingField
            label="AltMount URL"
            description="Optional: AltMount backend URL for authoritative completion status."
            hint="http://altmount:8080"
            value={form.getValue("virtual_library.altmount_url")}
            onChange={(v) => form.setValue("virtual_library.altmount_url", v)}
            restartRequired={restartKeys.has("virtual_library.altmount_url")}
          />
          <SecretField
            label="AltMount API key"
            hint="API key for authenticated AltMount status queries."
            value={form.getValue("virtual_library.altmount_api_key")}
            configured={form.sensitiveConfigured.includes("virtual_library.altmount_api_key")}
            onChange={(v) => form.setValue("virtual_library.altmount_api_key", v)}
            onKeep={() => form.resetValue("virtual_library.altmount_api_key")}
            // Nothing else on this page can empty the stored key, and an
            // AltMount instance without auth needs it empty.
            onClear={() => form.setValue("virtual_library.altmount_api_key", "")}
            cleared={form.isClearStaged("virtual_library.altmount_api_key")}
            restartRequired={restartKeys.has("virtual_library.altmount_api_key")}
          />
          <SettingField
            label="AltMount check interval (minutes)"
            type="number"
            description="How often to poll AltMount for completion updates. 1–10080."
            value={form.getValue("virtual_library.altmount_check_minutes") || "15"}
            onChange={(v) => form.setValue("virtual_library.altmount_check_minutes", v)}
            restartRequired={restartKeys.has("virtual_library.altmount_check_minutes")}
          />
        </FieldGroup>

        <FieldGroup label="Quality" dirty={anyDirty(QUALITY_KEYS)}>
          <VioScoringProfilesCard form={form} defaultExpanded={true} />
        </FieldGroup>
      </div>

      <SaveBar
        dirtyCount={form.dirtyCount}
        onSave={form.save}
        onDiscard={form.discard}
        isSaving={form.isSaving}
      />
    </div>
  );
}
