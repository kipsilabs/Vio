// @vitest-environment jsdom

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import type { PluginConfigSchema } from "@/api/types";

import { PluginConfigForm } from "./PluginConfigForm";

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

// jsdom lacks the pointer-capture API Radix Select calls when opening.
window.HTMLElement.prototype.hasPointerCapture ??= () => false;
window.HTMLElement.prototype.scrollIntoView ??= () => {};

vi.mock("@/hooks/queries/admin/libraries", () => ({
  useAdminLibraries: () => ({
    data: [{ id: 7, name: "Movies", type: "movie", enabled: true }],
  }),
}));

function renderWithClient(ui: React.ReactElement) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return {
    ...render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>),
    queryClient: client,
  };
}
const schema: PluginConfigSchema = {
  key: "account",
  title: "Account",
  json_schema: "{}",
  required: true,
  admin_form: {
    fields: [
      {
        key: "api_key",
        label: "API Key",
        control: "PASSWORD",
        required: false,
        secret: true,
        multiline: false,
      },
      {
        key: "region",
        label: "Region",
        control: "TEXT",
        required: false,
        secret: false,
        multiline: false,
      },
    ],
  },
};

describe("PluginConfigForm secrets", () => {
  it("leaves the title and border to the page panel when bare", () => {
    const schema = {
      key: "account",
      title: "Account title",
      description: "Account description",
      json_schema: JSON.stringify({ type: "object", properties: { token: { type: "string" } } }),
      required: false,
    };
    const { container, rerender } = renderWithClient(
      <PluginConfigForm schema={schema} onSave={vi.fn()} />,
    );
    expect(screen.getByText("Account title")).toBeInTheDocument();
    expect(container.querySelector("fieldset")).toHaveClass("border");

    rerender(
      <QueryClientProvider client={new QueryClient()}>
        <PluginConfigForm bare schema={schema} onSave={vi.fn()} />
      </QueryClientProvider>,
    );
    expect(screen.queryByText("Account title")).not.toBeInTheDocument();
    expect(screen.queryByText("Account description")).not.toBeInTheDocument();
    expect(container.querySelector("fieldset")).not.toHaveClass("border");
  });

  it("derives a form when a plugin only supplies JSON Schema", () => {
    renderWithClient(
      <PluginConfigForm
        schema={{
          key: "server",
          title: "Server",
          json_schema: JSON.stringify({
            type: "object",
            properties: {
              base_url: { type: "string", title: "Base URL" },
              api_key: { type: "string", format: "password" },
            },
            required: ["base_url", "api_key"],
          }),
          required: true,
        }}
        onSave={vi.fn()}
      />,
    );

    expect(screen.getByLabelText("Base URL")).toBeInTheDocument();
    expect(screen.getByLabelText("Api Key")).toHaveAttribute("type", "password");
  });

  it("renders an inferred library picker for a silo-library json_schema format", async () => {
    renderWithClient(
      <PluginConfigForm
        schema={{
          key: "virtual",
          title: "Virtual Library",
          json_schema: JSON.stringify({
            type: "object",
            properties: {
              movie_library_id: {
                type: "string",
                title: "Movie library",
                format: "silo-library-movie",
              },
            },
          }),
          required: true,
        }}
        onSave={vi.fn()}
      />,
    );

    await userEvent.click(screen.getByRole("combobox", { name: "Movie library" }));
    expect(await screen.findByRole("option", { name: "Movies (7)" })).toBeInTheDocument();
  });

  it("shows redacted saved state and only clears through an explicit action", async () => {
    const onSave = vi.fn();
    renderWithClient(
      <PluginConfigForm
        schema={schema}
        value={{ region: "us-east" }}
        configuredSecrets={["api_key"]}
        onSave={onSave}
      />,
    );

    expect(screen.getByLabelText("API Key")).toHaveAttribute(
      "placeholder",
      "Saved secret — leave blank to keep",
    );
    expect(screen.getByText("API Key: saved")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Save config" }));
    expect(onSave).toHaveBeenLastCalledWith(
      "account",
      expect.objectContaining({ region: "us-east" }),
      [],
    );

    await userEvent.click(screen.getByRole("button", { name: "Clear saved secret" }));
    await userEvent.click(screen.getByRole("button", { name: "Save config" }));
    expect(onSave).toHaveBeenLastCalledWith(
      "account",
      expect.objectContaining({ region: "us-east" }),
      ["api_key"],
    );
  });

  it("sends an emptied field as an explicit clear so the stored value goes", async () => {
    const onSave = vi.fn();
    renderWithClient(
      <PluginConfigForm
        schema={schema}
        value={{ region: "us-east" }}
        configuredSecrets={["api_key"]}
        onSave={onSave}
      />,
    );

    await userEvent.clear(screen.getByLabelText("Region"));
    await userEvent.click(screen.getByRole("button", { name: "Save config" }));
    const [, value, clearSecrets] = onSave.mock.lastCall!;
    expect(value).toEqual({ region: "" });
    expect(clearSecrets).toEqual([]);
  });

  it("does not offer to clear a required saved secret into an invalid config", () => {
    const requiredSchema: PluginConfigSchema = {
      ...schema,
      admin_form: {
        ...schema.admin_form!,
        fields: schema.admin_form!.fields.map((field) =>
          field.key === "api_key" ? { ...field, required: true } : field,
        ),
      },
    };
    renderWithClient(
      <PluginConfigForm
        schema={requiredSchema}
        value={{ region: "us-east" }}
        configuredSecrets={["api_key"]}
        onSave={vi.fn()}
      />,
    );

    expect(screen.getByText("API Key: saved (required)")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Clear saved secret" })).not.toBeInTheDocument();
  });

  it("keeps the submitted snapshot immutable while a save is pending", () => {
    renderWithClient(
      <PluginConfigForm
        schema={schema}
        value={{ region: "us-east" }}
        configuredSecrets={["api_key"]}
        onSave={vi.fn()}
        isSaving
      />,
    );

    expect(screen.getByLabelText("Region")).toBeDisabled();
    expect(screen.getByRole("button", { name: "Save config" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Clear saved secret" })).toBeDisabled();
  });

  it("tests the exact draft including staged secret removals", async () => {
    const onTest = vi.fn().mockResolvedValue({
      success: false,
      message: "API key is required",
    });
    renderWithClient(
      <PluginConfigForm
        schema={schema}
        value={{ region: "us-east" }}
        configuredSecrets={["api_key"]}
        onSave={vi.fn()}
        onTest={onTest}
      />,
    );

    await userEvent.click(screen.getByRole("button", { name: "Clear saved secret" }));
    await userEvent.click(screen.getByRole("button", { name: "Check Connection" }));

    expect(onTest).toHaveBeenCalledWith("account", expect.objectContaining({ region: "us-east" }), [
      "api_key",
    ]);
  });

  it("reports each edit and staged secret removal as the entry it would save", async () => {
    const onDraftChange = vi.fn();
    renderWithClient(
      <PluginConfigForm
        schema={schema}
        value={{ region: "us-east" }}
        configuredSecrets={["api_key"]}
        onSave={vi.fn()}
        onDraftChange={onDraftChange}
      />,
    );

    await userEvent.clear(screen.getByLabelText("Region"));
    await userEvent.type(screen.getByLabelText("Region"), "eu");
    expect(onDraftChange).toHaveBeenLastCalledWith(
      "account",
      expect.objectContaining({ region: "eu" }),
      [],
    );

    await userEvent.click(screen.getByRole("button", { name: "Clear saved secret" }));
    expect(onDraftChange).toHaveBeenLastCalledWith(
      "account",
      expect.objectContaining({ region: "eu" }),
      ["api_key"],
    );
  });
});

describe("PluginConfigForm quality profiles", () => {
  it("accepts Go/RE2 inline flags without applying JavaScript regex rules", async () => {
    const qualitySchema: PluginConfigSchema = {
      key: "streaming",
      title: "Streaming",
      json_schema: JSON.stringify({
        type: "object",
        properties: {
          quality_profiles: {
            type: "array",
            items: { type: "object" },
          },
        },
      }),
      required: true,
      admin_form: {
        fields: [
          {
            key: "quality_profiles",
            label: "Quality Profiles",
            control: "TEXTAREA",
            required: false,
            secret: false,
            multiline: true,
          },
        ],
      },
    };
    renderWithClient(
      <PluginConfigForm
        schema={qualitySchema}
        value={{
          quality_profiles:
            '[{"label":"4K HDR","include_regex":"(?i)(2160p|4k)","exclude_regex":"(?i)(cam|ts)"}]',
        }}
        onSave={vi.fn()}
      />,
    );

    await userEvent.click(screen.getByRole("button", { name: "Validate profiles" }));

    expect(screen.getByText(/Valid JSON structure: 4K HDR/)).toBeInTheDocument();
    expect(screen.queryByText(/Invalid include_regex/)).not.toBeInTheDocument();
  });

  it("says what an empty input means instead of leaking a JSON parse exception", async () => {
    const qualitySchema: PluginConfigSchema = {
      key: "streaming",
      title: "Streaming",
      json_schema: JSON.stringify({
        type: "object",
        properties: {
          quality_profiles: {
            type: "array",
            items: { type: "object" },
          },
        },
      }),
      required: true,
      admin_form: {
        fields: [
          {
            key: "quality_profiles",
            label: "Quality Profiles",
            control: "TEXTAREA",
            required: false,
            secret: false,
            multiline: true,
          },
        ],
      },
    };
    renderWithClient(<PluginConfigForm schema={qualitySchema} value={{}} onSave={vi.fn()} />);

    await userEvent.click(screen.getByRole("button", { name: "Validate profiles" }));

    // JSON.parse("") throws "Unexpected end of JSON input"; the guard must
    // surface an actionable message instead.
    expect(
      screen.getByText(/No profiles entered\. Paste a JSON array of profiles first\./),
    ).toBeInTheDocument();
    expect(screen.queryByText(/Unexpected end of JSON/)).not.toBeInTheDocument();
  });
});
