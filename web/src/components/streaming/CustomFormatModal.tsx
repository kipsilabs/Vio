import { useState } from "react";
import { Sparkles } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";

import type { CategoryType, CustomFormatRule } from "./scoringPresets";

interface CustomFormatModalProps {
  isOpen: boolean;
  onClose: () => void;
  onSave: (format: CustomFormatRule) => void;
  editingFormat?: CustomFormatRule | null;
}

const DEFAULT_NEW_FORMAT: Omit<CustomFormatRule, "id"> = {
  name: "",
  category: "custom",
  pattern: "",
  patternType: "regex",
  score: 250,
  enabled: true,
  isCustom: true,
  invert: false,
  reject: false,
};

function formatFormState(editingFormat?: CustomFormatRule | null): Omit<CustomFormatRule, "id"> {
  if (!editingFormat) return DEFAULT_NEW_FORMAT;
  return {
    name: editingFormat.name,
    category: editingFormat.category,
    pattern: editingFormat.pattern,
    patternType: editingFormat.patternType,
    score: editingFormat.score,
    enabled: editingFormat.enabled,
    isCustom: true,
    invert: editingFormat.invert ?? false,
    reject: editingFormat.reject ?? editingFormat.score <= -1500,
  };
}

export function CustomFormatModal(props: CustomFormatModalProps) {
  // A fresh editor is a fresh form: remount when the dialog opens or its
  // editing target changes instead of copying props into state in an effect.
  return (
    <CustomFormatModalBody key={`${props.isOpen}:${props.editingFormat?.id ?? "new"}`} {...props} />
  );
}

function CustomFormatModalBody({ isOpen, onClose, onSave, editingFormat }: CustomFormatModalProps) {
  const [formData, setFormData] = useState<Omit<CustomFormatRule, "id">>(() =>
    formatFormState(editingFormat),
  );
  const [patternError, setPatternError] = useState<string | null>(null);

  const validatePattern = (pattern: string, type: "regex" | "token") => {
    if (type === "regex" && pattern.trim()) {
      try {
        new RegExp(pattern);
        setPatternError(null);
      } catch (e) {
        setPatternError(e instanceof Error ? e.message : "Invalid Regular Expression");
      }
    } else {
      setPatternError(null);
    }
  };

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!formData.name.trim() || !formData.pattern.trim() || patternError) return;

    const formatId = editingFormat
      ? editingFormat.id
      : `custom_${Date.now()}_${Math.random().toString(36).substring(2, 6)}`;
    onSave({
      id: formatId,
      ...formData,
      name: formData.name.trim(),
      pattern: formData.pattern.trim(),
    });
    onClose();
  };

  return (
    <Dialog open={isOpen} onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <div className="flex items-center gap-2.5">
            <div className="bg-primary/10 text-primary flex size-9 items-center justify-center rounded-lg">
              <Sparkles className="size-4" />
            </div>
            <div>
              <DialogTitle>
                {editingFormat ? "Edit Custom Format" : "Add VIO Custom Format"}
              </DialogTitle>
              <DialogDescription>
                Define custom regex or keyword pattern rules for stream ranking.
              </DialogDescription>
            </div>
          </div>
        </DialogHeader>

        <form onSubmit={handleSubmit} className="space-y-4 pt-2">
          <div className="space-y-1.5">
            <Label htmlFor="cf-name" className="text-xs font-semibold">
              Rule Name
            </Label>
            <Input
              id="cf-name"
              placeholder="e.g. 4K Disc Remux (Untouched)"
              value={formData.name}
              required
              onChange={(e) => setFormData((p) => ({ ...p, name: e.target.value }))}
            />
          </div>

          <div className="grid grid-cols-2 gap-4">
            <div className="space-y-1.5">
              <Label htmlFor="cf-category" className="text-xs font-semibold">
                Category
              </Label>
              <Select
                value={formData.category}
                onValueChange={(val) =>
                  setFormData((p) => ({ ...p, category: val as CategoryType }))
                }
              >
                <SelectTrigger id="cf-category">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="source">Source (Disc/Remux/WEB)</SelectItem>
                  <SelectItem value="hdr">HDR (DV / HDR10+)</SelectItem>
                  <SelectItem value="audio">Audio (TrueHD / Atmos / DTS)</SelectItem>
                  <SelectItem value="release_group">Release Group</SelectItem>
                  <SelectItem value="resolution">Resolution</SelectItem>
                  <SelectItem value="custom">Custom Tag</SelectItem>
                </SelectContent>
              </Select>
            </div>

            <div className="space-y-1.5">
              <Label htmlFor="cf-type" className="text-xs font-semibold">
                Pattern Type
              </Label>
              <Select
                value={formData.patternType}
                onValueChange={(val) => {
                  const type = val as "regex" | "token";
                  setFormData((p) => ({ ...p, patternType: type }));
                  validatePattern(formData.pattern, type);
                }}
              >
                <SelectTrigger id="cf-type">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="regex">Regular Expression (Regex)</SelectItem>
                  <SelectItem value="token">Exact Substring / Token</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="cf-pattern" className="text-xs font-semibold">
              Match Pattern
            </Label>
            <Input
              id="cf-pattern"
              className="font-mono text-xs"
              placeholder={
                formData.patternType === "regex"
                  ? "\\b(remux|bdremux)\\b.*\\b(2160p|4k)\\b"
                  : "REMUX"
              }
              value={formData.pattern}
              required
              onChange={(e) => {
                const val = e.target.value;
                setFormData((p) => ({ ...p, pattern: val }));
                validatePattern(val, formData.patternType);
              }}
            />
            {patternError ? (
              <p className="text-destructive text-xs">{patternError}</p>
            ) : (
              <p className="text-muted-foreground text-[11px]">
                {formData.patternType === "regex"
                  ? "Standard case-insensitive Go/RE2 regular expression."
                  : "Matches word-boundary tokens without regex syntax."}
              </p>
            )}
          </div>

          <div className="space-y-2">
            <div className="flex items-center justify-between">
              <Label htmlFor="cf-score" className="text-xs font-semibold">
                Score Weight
              </Label>
              <span
                className={`font-mono text-xs font-semibold ${
                  formData.reject || formData.score <= -1500
                    ? "text-destructive"
                    : formData.score > 0
                      ? "text-green-600 dark:text-green-400"
                      : "text-amber-600 dark:text-amber-400"
                }`}
              >
                {formData.reject || formData.score <= -1500
                  ? "🚫 Discard (-2000)"
                  : `${formData.score > 0 ? `+${formData.score}` : formData.score} pts`}
              </span>
            </div>
            <Input
              id="cf-score"
              type="number"
              min={-2000}
              max={2000}
              step={25}
              value={formData.score}
              disabled={formData.reject}
              onChange={(e) => {
                const score = Number(e.target.value);
                setFormData((p) => ({ ...p, score }));
              }}
            />
          </div>

          <div className="bg-muted/40 flex items-center justify-between rounded-lg border p-3">
            <div>
              <Label htmlFor="cf-discard" className="text-xs font-semibold">
                Discard Title on Match
              </Label>
              <p className="text-muted-foreground text-[11px]">
                Instantly disqualify any candidate release matching this pattern.
              </p>
            </div>
            <Switch
              id="cf-discard"
              checked={formData.reject || formData.score <= -1500}
              onCheckedChange={(val) => {
                setFormData((p) => ({
                  ...p,
                  reject: val,
                  score: val ? -2000 : p.score <= -1500 ? -100 : p.score,
                }));
              }}
            />
          </div>

          <div className="bg-muted/40 flex items-center justify-between rounded-lg border p-3">
            <div>
              <Label htmlFor="cf-invert" className="text-xs font-semibold">
                Invert Match Condition
              </Label>
              <p className="text-muted-foreground text-[11px]">
                Score applies when pattern does NOT match the release name.
              </p>
            </div>
            <Switch
              id="cf-invert"
              checked={formData.invert ?? false}
              onCheckedChange={(val) => setFormData((p) => ({ ...p, invert: val }))}
            />
          </div>

          <DialogFooter className="pt-2">
            <Button type="button" variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button
              type="submit"
              disabled={Boolean(patternError) || !formData.name.trim() || !formData.pattern.trim()}
            >
              Save Rule
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
