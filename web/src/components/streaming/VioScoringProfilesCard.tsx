import { useMemo, useState } from "react";
import {
  ChevronDown,
  ChevronUp,
  Code,
  Crown,
  Edit2,
  Plus,
  RotateCcw,
  Sliders,
  Trash2,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Slider } from "@/components/ui/slider";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

import { CustomFormatModal } from "./CustomFormatModal";
import { QualityProfileModal } from "./QualityProfileModal";
import { SortCriteriaSummary } from "./SortCriteriaSummary";
import {
  type CategoryType,
  type CustomFormatRule,
  DEFAULT_QUALITY_PRESET_PROFILES,
  getDefaultFormatsForPreset,
  type QualityProfileRule,
} from "./scoringPresets";

interface FormBinding {
  getValue: (key: string) => string | undefined;
  setValue: (key: string, value: string) => void;
  isDirty?: (key: string) => boolean;
}

interface VioScoringProfilesCardProps {
  form: FormBinding;
  disabled?: boolean;
  defaultExpanded?: boolean;
}

const CATEGORY_COLORS: Record<CategoryType, string> = {
  source: "bg-blue-500/15 text-blue-700 dark:text-blue-300 border-blue-500/20",
  hdr: "bg-purple-500/15 text-purple-700 dark:text-purple-300 border-purple-500/20",
  audio: "bg-emerald-500/15 text-emerald-700 dark:text-emerald-300 border-emerald-500/20",
  release_group: "bg-amber-500/15 text-amber-700 dark:text-amber-300 border-amber-500/20",
  resolution: "bg-indigo-500/15 text-indigo-700 dark:text-indigo-300 border-indigo-500/20",
  custom: "bg-zinc-500/15 text-zinc-700 dark:text-zinc-300 border-zinc-500/20",
};

export function VioScoringProfilesCard({
  form,
  disabled = false,
  defaultExpanded = false,
}: VioScoringProfilesCardProps) {
  const [isExpanded, setIsExpanded] = useState(defaultExpanded);
  const [activeCategory, setActiveCategory] = useState<string>("all");
  const [showFormatModal, setShowFormatModal] = useState(false);
  const [editingFormat, setEditingFormat] = useState<CustomFormatRule | null>(null);

  const [showProfileModal, setShowProfileModal] = useState(false);
  const [editingProfile, setEditingProfile] = useState<QualityProfileRule | null>(null);
  const [rawProfilesMode, setRawProfilesMode] = useState(false);
  const [profileValidationMsg, setProfileValidationMsg] = useState<string | null>(null);

  // Parse custom formats from form, or fallback to preset defaults
  const customFormatPreset =
    form.getValue("virtual_library.custom_format_preset") || "altmount-recommended";
  const rawFormats = form.getValue("virtual_library.custom_formats");
  const trustServerOrder = customFormatPreset === "server-order";

  const formats: CustomFormatRule[] = useMemo(() => {
    if (rawFormats && rawFormats.trim()) {
      try {
        const parsed = JSON.parse(rawFormats);
        if (Array.isArray(parsed) && parsed.length > 0) {
          return parsed;
        }
      } catch {
        // Fall back to preset
      }
    }
    return getDefaultFormatsForPreset(customFormatPreset);
  }, [rawFormats, customFormatPreset]);

  // Parse quality profiles from form, or fallback to preset defaults
  const qualityPreset = form.getValue("virtual_library.quality_preset") || "balanced";
  const rawQualityProfiles = form.getValue("virtual_library.quality_profiles");
  const enableProfiles = form.getValue("virtual_library.enable_quality_profiles") === "true";

  const profiles: QualityProfileRule[] = useMemo(() => {
    if (rawQualityProfiles && rawQualityProfiles.trim()) {
      try {
        const parsed = JSON.parse(rawQualityProfiles);
        if (Array.isArray(parsed) && parsed.length > 0) {
          return parsed;
        }
      } catch {
        // Fall back to preset
      }
    }
    return (
      DEFAULT_QUALITY_PRESET_PROFILES[qualityPreset] ??
      DEFAULT_QUALITY_PRESET_PROFILES.balanced ??
      []
    );
  }, [rawQualityProfiles, qualityPreset]);

  // Update formats helper
  const updateFormats = (next: CustomFormatRule[], newPreset = "custom") => {
    form.setValue("virtual_library.custom_formats", JSON.stringify(next));
    form.setValue("virtual_library.custom_format_preset", newPreset);
  };

  // Update profiles helper
  const updateProfiles = (next: QualityProfileRule[], newPreset = "custom") => {
    form.setValue("virtual_library.quality_profiles", JSON.stringify(next));
    form.setValue("virtual_library.quality_preset", newPreset);
  };

  const handlePresetChange = (newPreset: string) => {
    if (newPreset === "custom" || newPreset === "server-order") {
      form.setValue("virtual_library.custom_format_preset", newPreset);
      return;
    }
    const defaultFormats = getDefaultFormatsForPreset(newPreset);
    const userCustoms = formats.filter((f) => f.isCustom);
    updateFormats([...defaultFormats, ...userCustoms], newPreset);
  };

  const handleQualityPresetChange = (newPreset: string) => {
    if (newPreset === "custom") {
      form.setValue("virtual_library.quality_preset", "custom");
      return;
    }
    const defaultProfiles =
      DEFAULT_QUALITY_PRESET_PROFILES[newPreset] ?? DEFAULT_QUALITY_PRESET_PROFILES.balanced ?? [];
    updateProfiles(defaultProfiles, newPreset);
  };

  const handleFormatToggle = (id: string, enabled: boolean) => {
    const updated = formats.map((f) => (f.id === id ? { ...f, enabled } : f));
    updateFormats(updated);
  };

  const handleFormatScoreChange = (id: string, score: number) => {
    const updated = formats.map((f) => (f.id === id ? { ...f, score, reject: score <= -1500 } : f));
    updateFormats(updated);
  };

  const handleDeleteFormat = (id: string) => {
    const updated = formats.filter((f) => f.id !== id);
    updateFormats(updated);
  };

  const handleSaveModalFormat = (format: CustomFormatRule) => {
    const existing = formats.some((f) => f.id === format.id);
    let updated: CustomFormatRule[];
    if (existing) {
      updated = formats.map((f) => (f.id === format.id ? format : f));
    } else {
      updated = [...formats, format];
    }
    updateFormats(updated);
  };

  const handleSaveModalProfile = (profile: QualityProfileRule) => {
    const existingIndex = profiles.findIndex((p) => p.label === profile.label);
    let updated: QualityProfileRule[];
    if (existingIndex >= 0) {
      updated = [...profiles];
      updated[existingIndex] = profile;
    } else {
      updated = [...profiles, profile];
    }
    updated.sort((a, b) => a.preferred_order - b.preferred_order);
    updateProfiles(updated);
  };

  const handleDeleteProfile = (label: string) => {
    const updated = profiles.filter((p) => p.label !== label);
    updateProfiles(updated);
  };

  const filteredFormats = useMemo(() => {
    if (activeCategory === "all") return formats;
    if (activeCategory === "custom") return formats.filter((f) => f.isCustom);
    return formats.filter((f) => f.category === activeCategory);
  }, [formats, activeCategory]);

  return (
    <div className="bg-card/70 border-border/80 min-w-0 overflow-hidden rounded-2xl border p-5 shadow-sm backdrop-blur-sm sm:p-6">
      {/* Header */}
      <div
        className={`flex flex-wrap items-center justify-between gap-4 ${isExpanded ? "border-border/60 border-b pb-4" : ""}`}
      >
        <div className="flex items-center gap-3">
          <div className="bg-primary/10 text-primary flex size-9 items-center justify-center rounded-xl shadow-xs">
            <Crown className="size-4.5 text-amber-500 dark:text-amber-400" />
          </div>
          <div>
            <div className="flex items-center gap-2">
              <h3 className="text-base font-bold tracking-tight">
                VIO Format Scoring & Quality Profiles
              </h3>
              <Badge
                variant={enableProfiles ? "default" : "outline"}
                className="text-[10px] font-semibold"
              >
                {enableProfiles ? "Active" : "Disabled"}
              </Badge>
            </div>
            <p className="text-muted-foreground text-xs">
              Automated point ranking engine for 4K Remux, Dolby Vision, HDR10+, lossless audio, and
              releases
            </p>
          </div>
        </div>

        <div className="flex items-center gap-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={disabled}
            onClick={() => setIsExpanded((v) => !v)}
          >
            {isExpanded ? (
              <>
                Hide
                <ChevronUp className="ml-1.5 size-3.5" />
              </>
            ) : (
              <>
                Advanced
                <ChevronDown className="ml-1.5 size-3.5" />
              </>
            )}
          </Button>
          {isExpanded && (
            <>
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={disabled}
                onClick={() => {
                  setEditingProfile(null);
                  setShowProfileModal(true);
                }}
              >
                <Sliders className="mr-1.5 size-3.5" />
                Add Profile
              </Button>
              <Button
                type="button"
                variant="default"
                size="sm"
                disabled={disabled || trustServerOrder}
                onClick={() => {
                  setEditingFormat(null);
                  setShowFormatModal(true);
                }}
              >
                <Plus className="mr-1.5 size-3.5" />
                Add Format
              </Button>
            </>
          )}
        </div>
      </div>

      {isExpanded && (
        <Tabs defaultValue="scoring" className="mt-5">
          <TabsList className="bg-muted/60 mb-5">
            <TabsTrigger value="scoring" className="text-xs">
              Format Scoring (VIO Engine)
            </TabsTrigger>
            <TabsTrigger value="profiles" className="text-xs">
              Quality Profiles & Resolution
            </TabsTrigger>
            <TabsTrigger value="streaming" className="text-xs">
              Stream Selection
            </TabsTrigger>
          </TabsList>

          {/* Tab 1: Format Scoring */}
          <TabsContent
            value="scoring"
            forceMount
            className="space-y-5 data-[state=inactive]:hidden"
          >
            <div className="bg-muted/30 flex flex-col gap-3 rounded-xl border p-4 sm:flex-row sm:items-center sm:justify-between">
              <div>
                <Label htmlFor="custom-format-preset" className="text-xs font-semibold">
                  Custom format preset
                </Label>
                <p className="text-muted-foreground text-xs">
                  Choose format scoring or keep the Stremio server’s stream order.
                </p>
              </div>
              <Select
                value={customFormatPreset}
                onValueChange={handlePresetChange}
                disabled={disabled}
              >
                <SelectTrigger
                  id="custom-format-preset"
                  aria-label="Custom format preset"
                  className="w-full sm:w-80"
                >
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="server-order">Trust server order</SelectItem>
                  <SelectItem value="altmount-recommended">
                    🏆 Vio Recommended (Remux & HDR Priority)
                  </SelectItem>
                  <SelectItem value="altmount-remux">
                    💎 4K Remux Enthusiast (Disc Focus)
                  </SelectItem>
                  <SelectItem value="altmount-compatibility">
                    📱 Compatibility (Universal Direct Play)
                  </SelectItem>
                  <SelectItem value="custom">🛠️ Custom User-Defined Weights</SelectItem>
                </SelectContent>
              </Select>
            </div>

            {trustServerOrder ? (
              <p className="text-muted-foreground text-sm">
                Keep streams in the order returned by the Stremio server. Format scores, profile
                sorting, and confirmed-source priority do not reorder streams. Quality profile
                filters still apply.
              </p>
            ) : (
              <>
                {/* Category Filter Pills */}
                <div className="flex flex-wrap items-center gap-1.5 border-b pb-3">
                  {[
                    { id: "all", label: "All Formats" },
                    { id: "source", label: "Source" },
                    { id: "hdr", label: "HDR" },
                    { id: "audio", label: "Audio" },
                    { id: "release_group", label: "Release Group" },
                    { id: "resolution", label: "Resolution" },
                    { id: "custom", label: "Custom Rules" },
                  ].map((tab) => (
                    <button
                      key={tab.id}
                      type="button"
                      onClick={() => setActiveCategory(tab.id)}
                      className={`rounded-lg px-2.5 py-1 text-xs font-medium transition-colors ${
                        activeCategory === tab.id
                          ? "bg-primary text-primary-foreground shadow-xs"
                          : "text-muted-foreground hover:bg-muted hover:text-foreground"
                      }`}
                    >
                      {tab.label}
                    </button>
                  ))}
                </div>

                {/* Table */}
                <div className="border-border/70 overflow-x-auto rounded-xl border">
                  <table className="w-full text-left text-xs">
                    <thead className="bg-muted/60 text-muted-foreground border-b text-[11px] font-semibold tracking-wider uppercase">
                      <tr>
                        <th className="w-12 px-3 py-2.5 text-center">Active</th>
                        <th className="min-w-40 px-3 py-2.5">Custom Format Tag</th>
                        <th className="w-28 px-3 py-2.5">Category</th>
                        <th className="w-28 px-3 py-2.5 text-center">Score</th>
                        <th className="min-w-48 px-3 py-2.5">Weight Adjustment</th>
                        <th className="w-16 px-3 py-2.5 text-right">Actions</th>
                      </tr>
                    </thead>
                    <tbody className="divide-border/40 divide-y">
                      {filteredFormats.length === 0 ? (
                        <tr>
                          <td
                            colSpan={6}
                            className="text-muted-foreground py-8 text-center text-xs"
                          >
                            No custom formats found in this category.
                          </td>
                        </tr>
                      ) : (
                        filteredFormats.map((format) => {
                          const isDiscard = format.score <= -1500 || format.reject;
                          return (
                            <tr
                              key={format.id}
                              className={`hover:bg-muted/30 transition-colors ${
                                !format.enabled ? "opacity-45" : ""
                              }`}
                            >
                              <td className="px-3 py-2 text-center">
                                <Switch
                                  checked={format.enabled}
                                  disabled={disabled}
                                  onCheckedChange={(val) => handleFormatToggle(format.id, val)}
                                />
                              </td>
                              <td className="px-3 py-2">
                                <div className="font-semibold">{format.name}</div>
                                <div className="text-muted-foreground max-w-xs truncate font-mono text-[10px]">
                                  {format.pattern}
                                </div>
                              </td>
                              <td className="px-3 py-2">
                                <span
                                  className={`inline-flex rounded-md border px-2 py-0.5 text-[10px] font-semibold tracking-wider uppercase ${
                                    CATEGORY_COLORS[format.category] || CATEGORY_COLORS.custom
                                  }`}
                                >
                                  {format.category.replace("_", " ")}
                                </span>
                              </td>
                              <td className="px-3 py-2 text-center">
                                {isDiscard ? (
                                  <Badge
                                    variant="destructive"
                                    className="text-[10px] font-semibold"
                                  >
                                    🚫 Discard
                                  </Badge>
                                ) : (
                                  <Badge
                                    variant={
                                      format.score > 0
                                        ? "default"
                                        : format.score < 0
                                          ? "secondary"
                                          : "outline"
                                    }
                                    className={`font-mono text-[11px] font-semibold ${
                                      format.score > 0
                                        ? "bg-green-600/15 text-green-700 hover:bg-green-600/20 dark:text-green-300"
                                        : format.score < 0
                                          ? "bg-amber-600/15 text-amber-700 hover:bg-amber-600/20 dark:text-amber-300"
                                          : ""
                                    }`}
                                  >
                                    {format.score > 0 ? `+${format.score}` : format.score} pts
                                  </Badge>
                                )}
                              </td>
                              <td className="px-3 py-2">
                                <div className="flex items-center gap-3">
                                  <Slider
                                    min={-2000}
                                    max={1000}
                                    step={25}
                                    value={[format.score]}
                                    disabled={disabled || !format.enabled}
                                    onValueChange={([val]) =>
                                      handleFormatScoreChange(format.id, val ?? format.score)
                                    }
                                    className="w-36"
                                  />
                                  <span className="text-muted-foreground font-mono text-[10px]">
                                    {format.score}
                                  </span>
                                </div>
                              </td>
                              <td className="px-3 py-2 text-right">
                                <div className="flex items-center justify-end gap-1">
                                  <Button
                                    type="button"
                                    variant="ghost"
                                    size="sm"
                                    className="size-7 p-0"
                                    onClick={() => {
                                      setEditingFormat(format);
                                      setShowFormatModal(true);
                                    }}
                                    title="Edit rule"
                                  >
                                    <Edit2 className="size-3.5" />
                                  </Button>
                                  {format.isCustom && (
                                    <Button
                                      type="button"
                                      variant="ghost"
                                      size="sm"
                                      className="text-destructive hover:bg-destructive/10 size-7 p-0"
                                      onClick={() => handleDeleteFormat(format.id)}
                                      title="Delete custom rule"
                                    >
                                      <Trash2 className="size-3.5" />
                                    </Button>
                                  )}
                                </div>
                              </td>
                            </tr>
                          );
                        })
                      )}
                    </tbody>
                  </table>
                </div>
              </>
            )}
          </TabsContent>

          {/* Tab 2: Quality Profiles & Resolution */}
          <TabsContent
            value="profiles"
            forceMount
            className="space-y-5 data-[state=inactive]:hidden"
          >
            <div className="bg-muted/30 flex items-center justify-between rounded-xl border p-4">
              <div>
                <Label htmlFor="toggle-qp" className="text-xs font-semibold">
                  Enable Quality Profiles
                </Label>
                <p className="text-muted-foreground text-xs">
                  Rank and select candidate streams by resolution, HDR, and codec profiles instead
                  of raw provider order.
                </p>
              </div>
              <Switch
                id="toggle-qp"
                checked={enableProfiles}
                disabled={disabled}
                onCheckedChange={(val) =>
                  form.setValue("virtual_library.enable_quality_profiles", val ? "true" : "false")
                }
              />
            </div>

            <div className="bg-muted/30 flex flex-col gap-3 rounded-xl border p-4 sm:flex-row sm:items-center sm:justify-between">
              <div>
                <Label htmlFor="quality-preset" className="text-xs font-semibold">
                  Quality preset
                </Label>
                <p className="text-muted-foreground text-xs">
                  Curated profile ordering or customize individual profiles below.
                </p>
              </div>
              <Select
                value={qualityPreset}
                onValueChange={handleQualityPresetChange}
                disabled={disabled || !enableProfiles}
              >
                <SelectTrigger
                  id="quality-preset"
                  aria-label="Quality preset"
                  className="w-full sm:w-64"
                >
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="balanced">Balanced (1080p & 4K)</SelectItem>
                  <SelectItem value="4k-hdr">4K HDR Priority</SelectItem>
                  <SelectItem value="4k-dolby-vision">4K Dolby Vision Priority</SelectItem>
                  <SelectItem value="no-dolby-vision">4K HDR10 (No Dolby Vision)</SelectItem>
                  <SelectItem value="no-hdr">4K SDR (No HDR)</SelectItem>
                  <SelectItem value="compatibility">Compatibility (H.264/AAC)</SelectItem>
                  <SelectItem value="anime">Anime (WEB-DL / Fansub)</SelectItem>
                  <SelectItem value="custom">Custom (User-defined profiles)</SelectItem>
                </SelectContent>
              </Select>
            </div>

            <div className="flex items-center justify-between">
              <h4 className="text-muted-foreground text-xs font-semibold tracking-wider uppercase">
                Configured Profiles ({profiles.length})
              </h4>
              <div className="flex items-center gap-2">
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  onClick={() => setRawProfilesMode((v) => !v)}
                  className="text-xs"
                >
                  <Code className="mr-1.5 size-3.5" />
                  {rawProfilesMode ? "Visual Editor" : "JSON Editor"}
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => handleQualityPresetChange(qualityPreset)}
                  title="Reset to selected preset defaults"
                  className="text-xs"
                >
                  <RotateCcw className="mr-1.5 size-3.5" />
                  Reset Defaults
                </Button>
              </div>
            </div>

            {rawProfilesMode ? (
              <div className="space-y-2">
                <textarea
                  className="border-input placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-ring/50 flex w-full rounded-md border bg-transparent px-3 py-2 font-mono text-xs shadow-xs outline-none focus-visible:ring-[3px] disabled:cursor-not-allowed disabled:opacity-50"
                  rows={10}
                  value={
                    form.getValue("virtual_library.quality_profiles") ||
                    JSON.stringify(profiles, null, 2)
                  }
                  onChange={(e) => {
                    form.setValue("virtual_library.quality_profiles", e.target.value);
                    form.setValue("virtual_library.quality_preset", "custom");
                    setProfileValidationMsg(null);
                  }}
                  placeholder="Paste JSON array of QualityProfile objects..."
                />
                <div className="flex items-center justify-between">
                  <Button
                    type="button"
                    size="sm"
                    variant="outline"
                    onClick={() => {
                      const raw = form.getValue("virtual_library.quality_profiles");
                      try {
                        if (!raw || !raw.trim()) throw new Error("Profiles JSON is empty.");
                        const parsed = JSON.parse(raw);
                        if (!Array.isArray(parsed))
                          throw new Error("Profiles must be a JSON array.");
                        setProfileValidationMsg(
                          `Valid JSON structure with ${parsed.length} profiles.`,
                        );
                      } catch (err) {
                        setProfileValidationMsg(
                          err instanceof Error ? err.message : "Invalid JSON",
                        );
                      }
                    }}
                  >
                    Validate JSON
                  </Button>
                  {profileValidationMsg && (
                    <p className="text-muted-foreground text-xs">{profileValidationMsg}</p>
                  )}
                </div>
              </div>
            ) : (
              <div className="border-border/70 overflow-hidden rounded-xl border">
                <table className="w-full text-left text-xs">
                  <thead className="bg-muted/60 text-muted-foreground border-b text-[11px] font-semibold tracking-wider uppercase">
                    <tr>
                      <th className="w-16 px-3 py-2.5 text-center">Rank</th>
                      <th className="min-w-36 px-3 py-2.5">Profile Label</th>
                      <th className="w-24 px-3 py-2.5">Resolution</th>
                      <th className="w-24 px-3 py-2.5">HDR / Dynamic</th>
                      <th className="min-w-36 px-3 py-2.5">Codecs & Filters</th>
                      <th className="w-16 px-3 py-2.5 text-right">Actions</th>
                    </tr>
                  </thead>
                  <tbody className="divide-border/40 divide-y">
                    {profiles.map((p, idx) => (
                      <tr key={p.label || idx} className="hover:bg-muted/30 transition-colors">
                        <td className="px-3 py-2.5 text-center">
                          <Badge variant="outline" className="font-mono text-xs">
                            #{p.preferred_order ?? idx + 1}
                          </Badge>
                        </td>
                        <td className="px-3 py-2.5 font-semibold">{p.label}</td>
                        <td className="px-3 py-2.5">
                          <Badge variant="secondary" className="font-mono text-[11px]">
                            {p.resolution || "any"}
                          </Badge>
                        </td>
                        <td className="px-3 py-2.5">
                          {p.hdr ? (
                            <Badge variant="default" className="text-[10px] uppercase">
                              {p.hdr}
                            </Badge>
                          ) : p.exclude_hdr ? (
                            <Badge variant="destructive" className="text-[10px]">
                              No {p.exclude_hdr}
                            </Badge>
                          ) : (
                            <span className="text-muted-foreground text-[11px]">Any</span>
                          )}
                        </td>
                        <td className="px-3 py-2.5">
                          <div className="text-muted-foreground space-y-0.5 font-mono text-[10px]">
                            {p.codec_video && <div>Video: {p.codec_video}</div>}
                            {p.codec_audio && <div>Audio: {p.codec_audio}</div>}
                            {p.audio_channels && <div>Channels: {p.audio_channels}</div>}
                            {p.language && <div>Lang: {p.language}</div>}
                            {p.visual_tag && <div>Visual: {p.visual_tag}</div>}
                            {p.require_multi_audio && <div>Require MULTi</div>}
                            {(p.min_size_gb || p.max_size_gb || p.min_size || p.max_size) && (
                              <div>
                                Size:{" "}
                                {p.min_size_gb
                                  ? `${p.min_size_gb}GB`
                                  : p.min_size
                                    ? `${p.min_size / 1e9}GB`
                                    : "0"}
                                {" - "}
                                {p.max_size_gb
                                  ? `${p.max_size_gb}GB`
                                  : p.max_size
                                    ? `${p.max_size / 1e9}GB`
                                    : "∞"}
                              </div>
                            )}
                            {p.include_regex && <div>Match: {p.include_regex}</div>}
                            {p.exclude_regex && <div>Exclude: {p.exclude_regex}</div>}
                            {!p.codec_video &&
                              !p.codec_audio &&
                              !p.audio_channels &&
                              !p.language &&
                              !p.visual_tag &&
                              !p.require_multi_audio &&
                              !p.include_regex &&
                              !p.exclude_regex && (
                                <span className="text-muted-foreground">Standard</span>
                              )}
                          </div>
                          <div className="mt-1 flex flex-wrap items-center gap-1">
                            <span className="text-muted-foreground text-[10px] font-semibold tracking-wider uppercase">
                              Sort
                            </span>
                            <SortCriteriaSummary criteria={p.sort} />
                          </div>
                        </td>
                        <td className="px-3 py-2.5 text-right">
                          <div className="flex items-center justify-end gap-1">
                            <Button
                              type="button"
                              variant="ghost"
                              size="sm"
                              className="size-7 p-0"
                              onClick={() => {
                                setEditingProfile(p);
                                setShowProfileModal(true);
                              }}
                              title="Edit profile"
                            >
                              <Edit2 className="size-3.5" />
                            </Button>
                            <Button
                              type="button"
                              variant="ghost"
                              size="sm"
                              className="text-destructive hover:bg-destructive/10 size-7 p-0"
                              onClick={() => handleDeleteProfile(p.label)}
                              title="Delete profile"
                            >
                              <Trash2 className="size-3.5" />
                            </Button>
                          </div>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </TabsContent>

          {/* Tab 3: Stream Selection */}
          <TabsContent value="streaming" className="space-y-4">
            <div className="bg-muted/30 flex items-center justify-between rounded-xl border p-4">
              <div>
                <Label htmlFor="toggle-single-stream" className="text-xs font-semibold">
                  Single stream with failover
                </Label>
                <p className="text-muted-foreground text-xs">
                  Register one highest-scoring stream per title; fall over to next candidate
                  automatically on playback failure.
                </p>
              </div>
              <Switch
                id="toggle-single-stream"
                checked={form.getValue("virtual_library.single_stream_with_failover") !== "false"}
                disabled={disabled}
                onCheckedChange={(val) =>
                  form.setValue(
                    "virtual_library.single_stream_with_failover",
                    val ? "true" : "false",
                  )
                }
              />
            </div>

            <div className="bg-muted/30 flex items-center justify-between rounded-xl border p-4">
              <div>
                <Label htmlFor="toggle-fallback" className="text-xs font-semibold">
                  Fallback to any stream
                </Label>
                <p className="text-muted-foreground text-xs">
                  When no stream matches quality or format profiles, select the highest-scoring
                  candidate anyway rather than failing.
                </p>
              </div>
              <Switch
                id="toggle-fallback"
                checked={form.getValue("virtual_library.fallback_to_any_stream") === "true"}
                disabled={disabled}
                onCheckedChange={(val) =>
                  form.setValue("virtual_library.fallback_to_any_stream", val ? "true" : "false")
                }
              />
            </div>
          </TabsContent>
        </Tabs>
      )}

      <CustomFormatModal
        isOpen={showFormatModal}
        onClose={() => {
          setShowFormatModal(false);
          setEditingFormat(null);
        }}
        onSave={handleSaveModalFormat}
        editingFormat={editingFormat}
      />

      <QualityProfileModal
        isOpen={showProfileModal}
        onClose={() => {
          setShowProfileModal(false);
          setEditingProfile(null);
        }}
        onSave={handleSaveModalProfile}
        editingProfile={editingProfile}
        defaultOrder={profiles.length + 1}
      />
    </div>
  );
}
