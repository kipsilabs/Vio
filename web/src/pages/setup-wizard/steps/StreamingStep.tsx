import { useMemo, useState } from "react";
import { AlertTriangle, Loader2, Sparkles } from "lucide-react";

import {
  ConnectionCheckAction,
  useConnectionCheck,
} from "@/components/admin/ConnectionCheckAction";
import { VioScoringProfilesCard } from "@/components/streaming/VioScoringProfilesCard";
import { Button } from "@/components/ui/button";
import { useAdminLibraries, useCreateLibrary } from "@/hooks/queries/admin/libraries";
import { useSettingsForm } from "@/hooks/useSettingsForm";
import { SettingField } from "@/pages/admin-settings/SettingField";

import { StepFrame, StepSection, StepSkeleton } from "../StepFrame";
import { useStepSubmit, useStepSummary } from "../useStep";

const STREAMING_KEYS = [
  "virtual_library.enabled",
  "virtual_library.manifest_url",
  "virtual_library.movie_library_id",
  "virtual_library.series_library_id",
  "virtual_library.allow_insecure_http",
  "virtual_library.allow_private_streams",
  "virtual_library.cache_ttl_minutes",
  "virtual_library.tmdb_api_key",
  "virtual_library.enable_quality_profiles",
  "virtual_library.quality_preset",
  "virtual_library.custom_format_preset",
  "virtual_library.quality_profiles",
  "virtual_library.custom_formats",
  "virtual_library.single_stream_with_failover",
  "virtual_library.fallback_to_any_stream",
];

export function StreamingStep() {
  const form = useSettingsForm({ keys: useMemo(() => STREAMING_KEYS, []) });
  const { handleSubmit, busy, skip } = useStepSubmit("streaming", form, "Failed to save");
  const { data: libraries } = useAdminLibraries();
  const createLibrary = useCreateLibrary();
  const [creatingLibs, setCreatingLibs] = useState(false);

  const providerCheck = useConnectionCheck("virtual_library", form, STREAMING_KEYS);

  const enabled = form.getValue("virtual_library.enabled") === "true";
  const manifestUrl = form.getValue("virtual_library.manifest_url");
  useStepSummary("streaming", enabled && manifestUrl ? "Stremio provider set" : "Skipped");

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

  if (form.isPending) return <StepSkeleton rows={4} />;

  return (
    <StepFrame
      title="Streaming"
      lede="Vio streams from a Stremio addon provider. Paste your provider's manifest URL and Vio registers its catalog as virtual libraries — no files needed."
      onSubmit={handleSubmit}
      busy={busy}
      onSkip={skip}
      footnote="Virtual libraries hold zero-storage titles streamed on demand. Details live in Admin › Settings › Streaming."
    >
      <StepSection
        title="Stremio provider"
        caption="The tokenized addon URL ending in /manifest.json. HTTPS is required; allow local HTTP only for private hosts."
      >
        <SettingField
          label="Enable streaming"
          type="toggle"
          description="Fetch stream candidates from the configured Stremio provider."
          value={enabled ? "true" : "false"}
          onChange={(v) => form.setValue("virtual_library.enabled", v)}
        />
        {enabled && (
          <>
            <SettingField
              label="Manifest URL"
              type="text"
              description="Your streaming provider's manifest URL (ends in /manifest.json)."
              hint="https://…/manifest.json"
              value={manifestUrl}
              onChange={(v) => form.setValue("virtual_library.manifest_url", v)}
            />
            <ConnectionCheckAction
              onClick={providerCheck.run}
              result={providerCheck.result}
              isPending={providerCheck.isPending}
              disabled={!form.getValue("virtual_library.manifest_url")}
            />
            <SettingField
              label="Movies library"
              type={movieSelectOptions.length > 0 ? "select" : "number"}
              options={movieSelectOptions.length > 0 ? movieSelectOptions : undefined}
              description="Virtual movies library (virtual://movies) that holds provider titles."
              value={form.getValue("virtual_library.movie_library_id")}
              onChange={(v) => form.setValue("virtual_library.movie_library_id", v)}
            />
            <SettingField
              label="Series library"
              type={seriesSelectOptions.length > 0 ? "select" : "number"}
              options={seriesSelectOptions.length > 0 ? seriesSelectOptions : undefined}
              description="Virtual series library (virtual://series) that holds provider episodes."
              value={form.getValue("virtual_library.series_library_id")}
              onChange={(v) => form.setValue("virtual_library.series_library_id", v)}
            />
            <div className="border-border/60 flex flex-col gap-2 border-b pt-1 pb-3 sm:flex-row sm:items-center sm:justify-between">
              <div>
                <p className="text-xs font-medium">Auto-create zero-storage libraries</p>
                <p className="text-muted-foreground text-xs">
                  Quickly create Virtual Movies (virtual://movies) and Virtual Series
                  (virtual://series).
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
              label="TMDB API key"
              description="Optional: resolves TMDB IDs to provider IDs for broader coverage."
              value={form.getValue("virtual_library.tmdb_api_key")}
              onChange={(v) => form.setValue("virtual_library.tmdb_api_key", v)}
            />
            <SettingField
              label="Allow HTTP for local manifests"
              type="toggle"
              description="Permits http:// manifest URLs on private/local networks only. HTTPS remains required for public hosts."
              value={
                form.getValue("virtual_library.allow_insecure_http") === "true" ? "true" : "false"
              }
              onChange={(v) => form.setValue("virtual_library.allow_insecure_http", v)}
            />
            <SettingField
              label="Allow private network streams"
              type="toggle"
              description="Lets virtual-stream playback contact non-public addresses (localhost, LAN, link-local)."
              value={
                form.getValue("virtual_library.allow_private_streams") === "true" ? "true" : "false"
              }
              onChange={(v) => form.setValue("virtual_library.allow_private_streams", v)}
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

            <div className="pt-4">
              <VioScoringProfilesCard form={form} defaultExpanded={false} />
            </div>
          </>
        )}
      </StepSection>
    </StepFrame>
  );
}
