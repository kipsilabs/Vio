import { useEffect, useMemo, useRef, useState } from "react";

import type {
  ConnectionCheckResponse,
  PluginAdminForm,
  PluginAdminFormField,
  PluginConfigSchema,
} from "@/api/types";
import { ConnectionCheckAction } from "@/components/admin/ConnectionCheckAction";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { usePluginInstallationConfigOptions } from "@/hooks/queries/admin/plugins";

import {
  adminFormForConfigSchema,
  formValuesFromConfig,
  humanizeConfigKey,
} from "./configSchemaAdminForm";
import { SchemaForm } from "./SchemaForm";
import { buildSchemaValues, parseFieldTypes } from "./schemaFormUtils";
import type { BuildSchemaValuesOptions, SchemaOption } from "./schemaFormUtils";

type PluginConfigValue = Record<string, unknown>;

const EMPTY_FIELDS: PluginAdminFormField[] = [];
// Global config saves merge into the stored value, so an emptied field must
// be sent as an explicit clear or the stored value stays.
const SAVE_OPTIONS: BuildSchemaValuesOptions = { explicitClears: true };

type Props = {
  schema: PluginConfigSchema;
  value?: PluginConfigValue;
  configuredSecrets?: string[];
  /** Installation ID used to load dynamic SELECT options from the plugin. */
  installationId?: number;
  onSave: (key: string, value: PluginConfigValue, clearSecrets: string[]) => void;
  onTest?: (
    key: string,
    value: PluginConfigValue,
    clearSecrets: string[],
  ) => Promise<ConnectionCheckResponse>;
  isSaving?: boolean;
  isTesting?: boolean;
  /**
   * Reports each edit as the entry it would save, so a page can test staged
   * entries together (the Sign-in page's connection test).
   */
  onDraftChange?: (key: string, value: PluginConfigValue, clearSecrets: string[]) => void;
  /**
   * Leave out the form's own title, description, and border, for a page panel
   * that already shows them.
   */
  bare?: boolean;
  /**
   * Prefix for the field ids, schema.key by default. A page that shows the
   * same schema key for several plugins must pass one that tells them apart,
   * or the labels of one form point at another's inputs.
   */
  idPrefix?: string;
};

export function PluginConfigForm({
  schema,
  value,
  configuredSecrets = [],
  installationId,
  onSave,
  onTest,
  isSaving = false,
  isTesting = false,
  onDraftChange,
  bare = false,
  idPrefix,
}: Props) {
  const inferredDescriptor = useMemo(() => adminFormForConfigSchema(schema), [schema]);
  const fields = inferredDescriptor?.fields ?? EMPTY_FIELDS;
  const supported = inferredDescriptor != null;
  const fieldTypes = useMemo(() => parseFieldTypes(schema.json_schema), [schema.json_schema]);

  const descriptor = useMemo<PluginAdminForm>(() => {
    const base = inferredDescriptor ?? { fields };
    const configured = new Set(configuredSecrets);
    return {
      ...base,
      fields: base.fields.map((field) =>
        configured.has(field.key) && (field.secret || field.control === "PASSWORD")
          ? { ...field, placeholder: "Saved secret — leave blank to keep" }
          : field,
      ),
    };
  }, [configuredSecrets, fields, inferredDescriptor]);

  const [values, setValues] = useState<PluginConfigValue>(() =>
    formValuesFromConfig(fields, value),
  );
  const [testResult, setTestResult] = useState<ConnectionCheckResponse | null>(null);
  const [profilePreview, setProfilePreview] = useState<string | null>(null);
  const [clearSecrets, setClearSecrets] = useState<Set<string>>(new Set());

  // Dynamic SELECT options: loaded once on mount (and when installationId changes)
  // from the plugin's request_router.v1 ListConfigOptions gRPC method.
  const [dynamicOptions, setDynamicOptions] = useState<Record<string, SchemaOption[]>>({});
  const [optionsLoading, setOptionsLoading] = useState(false);
  const loadOptions = usePluginInstallationConfigOptions();
  const hasDynamicFields = useMemo(() => fields.some((f) => f.dynamic_options), [fields]);
  const loadOptionsRef = useRef(loadOptions);
  loadOptionsRef.current = loadOptions;

  useEffect(() => {
    if (!installationId || !hasDynamicFields) return;
    setOptionsLoading(true);
    loadOptionsRef.current
      .mutateAsync({ installationId })
      .then((loaded) => {
        setDynamicOptions(loaded ?? ({} as Record<string, SchemaOption[]>));
      })
      .catch(() => {
        // Silently swallow — fields degrade to empty dropdowns.
      })
      .finally(() => setOptionsLoading(false));
    // Re-run only when the installation changes, not on every render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [installationId, hasDynamicFields]);

  useEffect(() => {
    setValues(formValuesFromConfig(fields, value));
    setClearSecrets(new Set());
  }, [fields, value]);

  function handleChange(next: PluginConfigValue) {
    setTestResult(null);
    setProfilePreview(null);
    setValues(next);
    const updated = new Set(clearSecrets);
    for (const key of configuredSecrets) {
      const replacement = next[key];
      if (typeof replacement === "string" && replacement.trim() !== "") {
        updated.delete(key);
      }
    }
    setClearSecrets(updated);
    onDraftChange?.(
      schema.key,
      buildSchemaValues(descriptor, next, fieldTypes, SAVE_OPTIONS),
      Array.from(updated),
    );
  }

  function toggleClearSecret(key: string) {
    const updated = new Set(clearSecrets);
    if (updated.has(key)) updated.delete(key);
    else updated.add(key);
    setClearSecrets(updated);
    onDraftChange?.(
      schema.key,
      buildSchemaValues(descriptor, values, fieldTypes, SAVE_OPTIONS),
      Array.from(updated),
    );
  }

  async function handleTest() {
    if (!onTest) {
      return;
    }

    try {
      setTestResult(
        await onTest(
          schema.key,
          buildSchemaValues(descriptor, values, fieldTypes, SAVE_OPTIONS),
          Array.from(clearSecrets),
        ),
      );
    } catch (error) {
      setTestResult({
        success: false,
        message: error instanceof Error ? error.message : "Connection check failed.",
      });
    }
  }

  if (!supported) {
    return (
      <div className="space-y-2 rounded-md border border-amber-500/30 bg-amber-500/5 p-3">
        <Label>{schema.title || schema.key}</Label>
        <p className="text-muted-foreground text-sm">
          This plugin uses a configuration schema shape that the admin form does not support yet.
        </p>
      </div>
    );
  }

  return (
    <fieldset
      disabled={isSaving || isTesting}
      className={bare ? "space-y-3" : "space-y-3 rounded-md border p-3"}
    >
      {bare ? null : (
        <div className="space-y-1">
          <Label>{schema.title || schema.key}</Label>
          {schema.description ? (
            <p className="text-muted-foreground text-xs">{schema.description}</p>
          ) : null}
        </div>
      )}

      <SchemaForm
        descriptor={descriptor}
        values={values}
        onChange={handleChange}
        idPrefix={idPrefix ?? schema.key}
        dynamicOptions={dynamicOptions}
        optionsLoading={optionsLoading}
      />

      {fields.some((field) => field.key === "quality_profiles") ? (
        <div className="space-y-2 rounded-md border border-dashed p-2.5">
          <Button
            type="button"
            size="sm"
            variant="outline"
            onClick={() => {
              const raw = values.quality_profiles;
              try {
                // Empty input: JSON.parse("") throws the raw browser
                // "Unexpected end of JSON input" — say what it means instead.
                if (typeof raw === "string" && raw.trim() === "") {
                  throw new Error("No profiles entered. Paste a JSON array of profiles first.");
                }
                const parsed = typeof raw === "string" ? JSON.parse(raw) : raw;
                if (!Array.isArray(parsed)) throw new Error("Profiles must be a JSON array.");
                const seen = new Set<string>();
                const labels = parsed.map((profile, index) => {
                  if (
                    !profile ||
                    typeof profile !== "object" ||
                    typeof profile.label !== "string" ||
                    !profile.label.trim()
                  ) {
                    throw new Error(`Profile ${index + 1} must have a label.`);
                  }
                  const typed = profile as Record<string, unknown>;
                  const label = profile.label.trim();
                  const key = label.toLowerCase();
                  if (seen.has(key)) throw new Error(`Duplicate profile label: ${label}.`);
                  seen.add(key);
                  for (const regexKey of ["include_regex", "exclude_regex"]) {
                    const regex = typed[regexKey];
                    if (regex !== undefined && typeof regex !== "string") {
                      throw new Error(`${regexKey} in profile ${label} must be a string.`);
                    }
                  }
                  return label;
                });
                setProfilePreview(
                  `Valid JSON structure: ${labels.join(", ")}. Save or test the connection to validate Go/RE2 expressions.`,
                );
              } catch (error) {
                setProfilePreview(
                  error instanceof Error ? error.message : "Invalid profiles JSON.",
                );
              }
            }}
          >
            Validate profiles
          </Button>
          {profilePreview ? (
            <p className="text-muted-foreground text-xs">{profilePreview}</p>
          ) : null}
        </div>
      ) : null}

      {configuredSecrets.length > 0 ? (
        <div className="space-y-2 rounded-md border border-dashed p-2.5">
          {configuredSecrets.map((key) => {
            const field = fields.find((candidate) => candidate.key === key);
            const clearing = clearSecrets.has(key);
            const required = field?.required === true;
            return (
              <div key={key} className="flex items-center justify-between gap-3 text-xs">
                <span className={clearing ? "text-destructive" : "text-muted-foreground"}>
                  {field?.label || humanizeConfigKey(key)}: {clearing ? "will be cleared" : "saved"}
                  {required ? " (required)" : ""}
                </span>
                {!required ? (
                  <Button
                    type="button"
                    size="xs"
                    variant="ghost"
                    onClick={() => toggleClearSecret(key)}
                  >
                    {clearing ? "Keep saved secret" : "Clear saved secret"}
                  </Button>
                ) : null}
              </div>
            );
          })}
        </div>
      ) : null}

      <div className="flex flex-wrap items-center gap-3">
        {onTest ? (
          <ConnectionCheckAction
            onClick={handleTest}
            result={testResult}
            isPending={isTesting}
            disabled={isSaving}
          />
        ) : null}
        <Button
          size="sm"
          variant="outline"
          disabled={isSaving || isTesting}
          onClick={() =>
            onSave(
              schema.key,
              buildSchemaValues(descriptor, values, fieldTypes, SAVE_OPTIONS),
              Array.from(clearSecrets),
            )
          }
        >
          {schema.admin_form?.submit_label || "Save config"}
        </Button>
      </div>
    </fieldset>
  );
}
