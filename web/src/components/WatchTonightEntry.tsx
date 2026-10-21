import { lazy, Suspense, useState } from "react";
import { Sparkles } from "lucide-react";
import { Button } from "@/components/ui/button";

const WatchTonightDialog = lazy(() => import("./WatchTonightDialog"));

export default function WatchTonightEntry() {
  const [open, setOpen] = useState(false);
  return (
    <div className="page-shell flex flex-wrap items-center justify-between gap-4">
      <div>
        <h2 className="text-lg font-semibold">Watch Tonight</h2>
        <p className="text-muted-foreground text-sm">Continue a favorite or find something new.</p>
      </div>
      <Button variant="outline" onClick={() => setOpen(true)}>
        <Sparkles className="h-4 w-4" />
        Help me choose
      </Button>
      {open && (
        <Suspense fallback={<p role="status">Loading Watch Tonight…</p>}>
          <WatchTonightDialog open onOpenChange={setOpen} />
        </Suspense>
      )}
    </div>
  );
}
