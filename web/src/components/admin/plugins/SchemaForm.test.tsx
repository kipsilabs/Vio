import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Library, PluginAdminForm } from "@/api/types";
import { SchemaForm } from "./SchemaForm";
import { field } from "./adminFormField.test-support";

// jsdom lacks the pointer-capture API Radix Select calls when opening.
window.HTMLElement.prototype.hasPointerCapture ??= () => false;
window.HTMLElement.prototype.scrollIntoView ??= () => {};

const { librariesState, useAdminLibrariesCalls } = vi.hoisted(() => ({
  librariesState: { current: [] as Library[] },
  useAdminLibrariesCalls: [] as Array<{ enabled?: boolean } | undefined>,
}));
vi.mock("@/hooks/queries/admin/libraries", () => ({
  useAdminLibraries: (options?: { enabled?: boolean }) => {
    useAdminLibrariesCalls.push(options);
    return { data: librariesState.current };
  },
}));

function library(overrides: Partial<Library> & Pick<Library, "id" | "name" | "type">): Library {
  return {
    paths: [],
    enabled: true,
    metadata_language: "",
    auto_translate_metadata: false,
    chapter_thumbnails_enabled: false,
    chapter_thumbnails_supported: false,
    intro_detection_enabled: false,
    trailer_kinds: [],
    realtime_monitoring: false,
    sort_order: 0,
    last_scanned_at: null,
    ...overrides,
  };
}

const LIBRARIES: Library[] = [
  library({ id: 1, name: "Movies", type: "movie" }),
  library({ id: 3, name: "Shows", type: "series" }),
  library({ id: 5, name: "Mixed", type: "mixed" }),
  library({ id: 6, name: "Disabled movies", type: "movie", enabled: false }),
];

function libraryPickerDescriptor(picker: "any" | "movie" | "tv"): PluginAdminForm {
  return {
    fields: [
      {
        key: "library_id",
        label: "Library",
        control: "SELECT",
        required: false,
        secret: false,
        multiline: false,
        library_picker: picker,
      },
    ],
  };
}

beforeEach(() => {
  librariesState.current = [];
  useAdminLibrariesCalls.length = 0;
});

const descriptor: PluginAdminForm = {
  fields: [
    field("service_kind", "Service", "SELECT", {
      required: true,
      options: [
        { value: "radarr", label: "Radarr" },
        { value: "sonarr", label: "Sonarr" },
      ],
    }),
    field("season_folder", "Season folder", "SWITCH", {
      show_when: [{ field: "service_kind", equals: ["sonarr"] }],
    }),
    field("root_folder", "Root folder", "SELECT", { dynamic_options: true }),
  ],
  sections: [
    {
      key: "main",
      title: "Library",
      collapsible: false,
      collapsed_default: false,
      field_keys: ["service_kind", "season_folder", "root_folder"],
    },
  ],
};

function renderForm(
  values: Record<string, unknown>,
  extra: Partial<React.ComponentProps<typeof SchemaForm>> = {},
) {
  const onChange = vi.fn();
  render(<SchemaForm descriptor={descriptor} values={values} onChange={onChange} {...extra} />);
  return { onChange };
}

describe("SchemaForm", () => {
  it("hides a field whose show_when is unmet", () => {
    renderForm({ service_kind: "radarr" });
    expect(screen.queryByText("Season folder")).toBeNull();
  });
  it("shows a field whose show_when is met", () => {
    renderForm({ service_kind: "sonarr" });
    expect(screen.getByText("Season folder")).toBeTruthy();
  });
  it("renders a declared default_value when the field is absent from values (#6)", () => {
    const d: PluginAdminForm = {
      fields: [field("season_folder", "Season folder", "SWITCH", { default_value: true })],
    };
    const onChange = vi.fn();
    render(<SchemaForm descriptor={d} values={{}} onChange={onChange} />);
    expect((screen.getByRole("switch") as HTMLButtonElement).getAttribute("aria-checked")).toBe(
      "true",
    );
  });
  it("uses a controlling field default when rendering a conditional field", () => {
    const d: PluginAdminForm = {
      fields: [
        field("advanced_enabled", "Advanced", "SWITCH", { default_value: true }),
        field("endpoint", "Endpoint", "TEXT", {
          show_when: [{ field: "advanced_enabled", equals: ["true"] }],
        }),
      ],
    };
    render(<SchemaForm descriptor={d} values={{}} onChange={vi.fn()} />);
    expect(screen.getByText("Endpoint")).toBeTruthy();
  });
  it("reports validity through onValidityChange (#14)", () => {
    const onValidityChange = vi.fn();
    const d: PluginAdminForm = {
      fields: [field("name", "Name", "TEXT", { required: true })],
    };
    const { rerender } = render(
      <SchemaForm
        descriptor={d}
        values={{}}
        onChange={vi.fn()}
        onValidityChange={onValidityChange}
      />,
    );
    expect(onValidityChange).toHaveBeenCalledExactlyOnceWith(false);
    const replacementCallback = vi.fn();
    rerender(
      <SchemaForm
        descriptor={d}
        values={{}}
        onChange={vi.fn()}
        onValidityChange={replacementCallback}
      />,
    );
    expect(onValidityChange).toHaveBeenCalledTimes(1);
    expect(replacementCallback).not.toHaveBeenCalled();
    rerender(
      <SchemaForm
        descriptor={d}
        values={{ name: "ok" }}
        onChange={vi.fn()}
        onValidityChange={replacementCallback}
      />,
    );
    expect(replacementCallback).toHaveBeenCalledExactlyOnceWith(true);
    expect(onValidityChange).toHaveBeenCalledTimes(1);
  });
});

const collapsibleDescriptor: PluginAdminForm = {
  fields: [
    field("api_path", "API path", "TEXT", { required: true }),
    field("verbose", "Verbose", "SWITCH"),
  ],
  sections: [
    {
      key: "lib",
      title: "Library",
      collapsible: true,
      collapsed_default: true,
      field_keys: ["api_path", "verbose"],
    },
  ],
};

describe("SchemaForm collapsible sections", () => {
  it("honors collapsed_default when the section has no field errors", () => {
    render(
      <SchemaForm
        descriptor={collapsibleDescriptor}
        values={{ api_path: "/v3" }}
        onChange={vi.fn()}
      />,
    );
    expect(screen.queryByText("Verbose")).toBeNull(); // collapsed -> field hidden
    expect(screen.getByText("Show")).toBeTruthy();
  });

  it("auto-expands a collapsed section that has a validation error (empty required field)", () => {
    render(<SchemaForm descriptor={collapsibleDescriptor} values={{}} onChange={vi.fn()} />);
    // api_path is required + empty -> validateSchemaValues flags it -> section force-expands
    expect(screen.getByText("Verbose")).toBeTruthy();
  });

  it("uses a controlling field default when rendering a conditional section", () => {
    const d: PluginAdminForm = {
      fields: [
        field("advanced_enabled", "Advanced", "SWITCH", { default_value: true }),
        field("endpoint", "Endpoint", "TEXT"),
      ],
      sections: [
        {
          key: "advanced",
          title: "Advanced options",
          collapsible: false,
          collapsed_default: false,
          field_keys: ["endpoint"],
          show_when: [{ field: "advanced_enabled", equals: ["true"] }],
        },
      ],
    };
    render(<SchemaForm descriptor={d} values={{}} onChange={vi.fn()} />);
    expect(screen.getByText("Advanced options")).toBeTruthy();
    expect(screen.getByText("Endpoint")).toBeTruthy();
  });
});

it("marks a show_when-gated field as nested when it is revealed", () => {
  const d: PluginAdminForm = {
    fields: [
      {
        key: "service_kind",
        label: "Service",
        control: "SELECT",
        required: false,
        secret: false,
        multiline: false,
        options: [{ value: "sonarr", label: "Sonarr" }],
      },
      {
        key: "series_type",
        label: "Series type",
        control: "SELECT",
        required: false,
        secret: false,
        multiline: false,
        show_when: [{ field: "service_kind", equals: ["sonarr"] }],
        options: [{ value: "standard", label: "Standard" }],
      },
    ],
  };
  const { container } = render(
    <SchemaForm descriptor={d} values={{ service_kind: "sonarr" }} onChange={vi.fn()} />,
  );
  expect(container.querySelector('[data-nested="true"]')).not.toBeNull();
});

it("renders inputs with autocomplete and password-manager ignore attributes", () => {
  const d: PluginAdminForm = {
    fields: [
      {
        key: "manifest_url",
        label: "Manifest URL",
        control: "TEXT",
        required: false,
        secret: false,
        multiline: false,
      },
      {
        key: "api_key",
        label: "API Key",
        control: "PASSWORD",
        required: false,
        secret: true,
        multiline: false,
      },
    ],
  };
  const { container } = render(<SchemaForm descriptor={d} values={{}} onChange={vi.fn()} />);
  const textInput = container.querySelector("#schema-manifest_url");
  expect(textInput?.getAttribute("autocomplete")).toBe("off");
  expect(textInput?.getAttribute("data-1p-ignore")).toBe("true");

  const passwordInput = container.querySelector("#schema-api_key");
  expect(passwordInput?.getAttribute("autocomplete")).toBe("new-password");
  expect(passwordInput?.getAttribute("data-1p-ignore")).toBe("true");
});

describe("SchemaForm library picker", () => {
  it("requests libraries only when the descriptor has a library picker", () => {
    const { rerender } = render(
      <SchemaForm descriptor={descriptor} values={{}} onChange={vi.fn()} />,
    );
    expect(useAdminLibrariesCalls[0]).toEqual({ enabled: false });

    rerender(
      <SchemaForm descriptor={libraryPickerDescriptor("any")} values={{}} onChange={vi.fn()} />,
    );
    expect(useAdminLibrariesCalls[useAdminLibrariesCalls.length - 1]).toEqual({ enabled: true });
  });

  it("shows the selected library by name and id", () => {
    librariesState.current = LIBRARIES;
    render(
      <SchemaForm
        descriptor={libraryPickerDescriptor("any")}
        values={{ library_id: "3" }}
        onChange={vi.fn()}
      />,
    );
    expect(screen.getByText("Shows (3)")).toBeTruthy();
  });

  it("lists only movie and mixed libraries for a movie picker", async () => {
    librariesState.current = LIBRARIES;
    render(
      <SchemaForm descriptor={libraryPickerDescriptor("movie")} values={{}} onChange={vi.fn()} />,
    );
    await userEvent.click(screen.getByRole("combobox", { name: "Library" }));
    expect(await screen.findByRole("option", { name: "Movies (1)" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "Mixed (5)" })).toBeTruthy();
    expect(screen.queryByRole("option", { name: "Shows (3)" })).toBeNull();
    expect(screen.queryByRole("option", { name: "Disabled movies (6)" })).toBeNull();
  });

  it("keeps a saved value with no matching library as a not-found option", () => {
    librariesState.current = LIBRARIES;
    render(
      <SchemaForm
        descriptor={libraryPickerDescriptor("movie")}
        values={{ library_id: "99" }}
        onChange={vi.fn()}
      />,
    );
    expect(screen.getByText("99 (not found)")).toBeTruthy();
  });

  it("keeps a saved value while the library list is still empty", () => {
    librariesState.current = [];
    render(
      <SchemaForm
        descriptor={libraryPickerDescriptor("any")}
        values={{ library_id: "3" }}
        onChange={vi.fn()}
      />,
    );
    expect(screen.getByText("3 (not found)")).toBeTruthy();
  });
});

describe("SchemaForm host-owned fields", () => {
  const hostDescriptor: PluginAdminForm = {
    fields: [
      field("quality_profile_id", "Quality profile", "SELECT", {
        required: true,
        options: [{ value: "1", label: "HD-1080p" }],
      }),
      field("is_default", "Default (HD/1080p)", "SWITCH"),
      field("anime_enabled", "Enable anime overrides", "SWITCH"),
      field("anime_root_folder", "Anime root folder", "TEXT", { required: true }),
    ],
    sections: [
      {
        key: "library",
        title: "Library",
        collapsible: true,
        collapsed_default: true,
        field_keys: ["quality_profile_id", "is_default"],
      },
      {
        key: "anime",
        title: "Anime overrides",
        collapsible: false,
        collapsed_default: false,
        field_keys: ["anime_enabled", "anime_root_folder"],
      },
    ],
  };
  const hidden = ["is_default", "anime_enabled", "anime_root_folder"];

  it("hides host-owned keys and drops a section they leave empty", () => {
    render(
      <SchemaForm
        descriptor={hostDescriptor}
        values={{ quality_profile_id: "1", is_default: true }}
        onChange={vi.fn()}
        hiddenKeys={hidden}
        expandSections
      />,
    );
    expect(screen.queryByText("Default (HD/1080p)")).toBeNull();
    expect(screen.queryByText("Anime overrides")).toBeNull();
    expect(screen.queryByText("Anime root folder")).toBeNull();
    expect(screen.getByText("Quality profile")).toBeTruthy();
  });

  it("keeps hidden values when a visible field changes", () => {
    const onChange = vi.fn();
    render(
      <SchemaForm
        descriptor={{
          fields: [field("name", "Name", "TEXT"), hostDescriptor.fields[1]!],
        }}
        values={{ name: "a", is_default: true }}
        onChange={onChange}
        hiddenKeys={hidden}
      />,
    );
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "b" } });
    expect(onChange).toHaveBeenCalledWith({ name: "b", is_default: true });
  });

  it("leaves hidden required fields out of validation", () => {
    const onValidityChange = vi.fn();
    render(
      <SchemaForm
        descriptor={hostDescriptor}
        values={{ quality_profile_id: "1", anime_enabled: true }}
        onChange={vi.fn()}
        onValidityChange={onValidityChange}
        hiddenKeys={hidden}
      />,
    );
    expect(onValidityChange).toHaveBeenLastCalledWith(true);
  });
});

describe("SchemaForm empty-string select options", () => {
  const promptForm: PluginAdminForm = {
    fields: [
      field("prompt", "Prompt", "SELECT", {
        default_value: "",
        options: [
          { value: "", label: "Provider default" },
          { value: "login", label: "Always ask for credentials" },
        ],
      }),
    ],
  };

  it("shows and writes back an option whose value is empty", async () => {
    Object.defineProperties(Element.prototype, {
      hasPointerCapture: { configurable: true, value: () => false },
      setPointerCapture: { configurable: true, value: () => {} },
      releasePointerCapture: { configurable: true, value: () => {} },
      scrollIntoView: { configurable: true, value: () => {} },
    });
    const onChange = vi.fn();
    const { rerender } = render(
      <SchemaForm descriptor={promptForm} values={{ prompt: "" }} onChange={onChange} />,
    );
    expect(screen.getByRole("combobox")).toHaveTextContent("Provider default");

    rerender(
      <SchemaForm descriptor={promptForm} values={{ prompt: "login" }} onChange={onChange} />,
    );
    expect(screen.getByRole("combobox")).toHaveTextContent("Always ask for credentials");
    fireEvent.pointerDown(screen.getByRole("combobox"), {
      button: 0,
      ctrlKey: false,
      pointerType: "mouse",
    });
    fireEvent.click(await screen.findByRole("option", { name: "Provider default" }));
    expect(onChange).toHaveBeenLastCalledWith({ prompt: "" });
  });
});
