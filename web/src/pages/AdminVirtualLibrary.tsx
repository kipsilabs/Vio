import { useMemo, useState } from "react";
import { Link } from "react-router";
import { AlertTriangle, FolderPlus, Inbox, Loader2, RefreshCw, Search } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useCreateLibrary, useAdminLibraries } from "@/hooks/queries/admin/libraries";
import { useAdminVirtualItems } from "@/hooks/queries/admin/virtualItems";
import type { CreateLibraryRequest, Library } from "@/api/types";
import { formatRelativeTime } from "@/lib/date";
import { formatDateTime } from "@/lib/datetime";
import { cn } from "@/lib/utils";

const VIRTUAL_PATH_PREFIX = "virtual://";

type VirtualKind = "movies" | "series";

const CREATE_SPECS: Record<VirtualKind, CreateLibraryRequest> = {
  movies: { paths: ["virtual://movies"], type: "movies", name: "Virtual Movies" },
  series: { paths: ["virtual://series"], type: "series", name: "Virtual Series" },
};

const KIND_LABELS: Record<VirtualKind, string> = {
  movies: "Movies",
  series: "Series",
};

interface VirtualCoverage {
  hasMovies: boolean;
  hasSeries: boolean;
  movieNames: string[];
  seriesNames: string[];
}

/** A library is virtual when any of its paths uses the `virtual://` scheme. */
function isVirtualLibrary(library: Library): boolean {
  return library.paths.some((path) => path.startsWith(VIRTUAL_PATH_PREFIX));
}

/** The kinds a library type serves; both hold for a mixed library. */
function typeCapabilities(type: string): { movies: boolean; series: boolean } {
  const value = type.trim().toLowerCase();
  const movies = value === "movie" || value === "movies" || value === "mixed";
  const series =
    value === "series" ||
    value === "tv" ||
    value === "show" ||
    value === "tvshows" ||
    value === "mixed";
  return { movies, series };
}

function virtualCoverage(libraries: Library[] | undefined): VirtualCoverage {
  const virtualLibraries = (libraries ?? []).filter(isVirtualLibrary);
  const movieLibraries = virtualLibraries.filter(
    (library) => typeCapabilities(library.type).movies,
  );
  const seriesLibraries = virtualLibraries.filter(
    (library) => typeCapabilities(library.type).series,
  );
  return {
    hasMovies: movieLibraries.length > 0,
    hasSeries: seriesLibraries.length > 0,
    movieNames: movieLibraries.map((library) => library.name),
    seriesNames: seriesLibraries.map((library) => library.name),
  };
}

function typeLabel(type: string): string {
  if (type === "movie") return "Movie";
  if (type === "series") return "Series";
  return type;
}

function pluginLabel(installationId: string): string {
  return installationId === "0" ? "Host" : `Plugin #${installationId}`;
}

function RelativeCell({ value }: { value?: string }) {
  // The two instants are absent rather than null when an item was never
  // delivered or never seen, so there is nothing to format in that case.
  if (!value) {
    return <span className="text-muted-foreground">Never</span>;
  }
  const relative = formatRelativeTime(value, { rounding: "floor" });
  if (!relative) {
    return <span className="text-muted-foreground">Never</span>;
  }
  return <span title={formatDateTime(value)}>{relative}</span>;
}

function SetupCard({
  coverage,
  creating,
  onCreate,
}: {
  coverage: VirtualCoverage;
  creating: VirtualKind | null;
  onCreate: (kind: VirtualKind) => void;
}) {
  return (
    <Card className="surface-panel rounded-2xl border-0">
      <CardHeader className="space-y-1">
        <CardTitle className="text-sm font-bold">Virtual library setup</CardTitle>
        <p className="text-muted-foreground text-xs">
          A virtual library holds zero-storage items that a plugin registers and streams on demand.
          Nothing is stored on disk until something plays.
        </p>
      </CardHeader>
      <CardContent className="grid gap-3 sm:grid-cols-2">
        {(["movies", "series"] as const).map((kind) => {
          const names = kind === "movies" ? coverage.movieNames : coverage.seriesNames;
          const spec = CREATE_SPECS[kind];
          const isCreating = creating === kind;
          return (
            <div
              key={kind}
              className="border-border flex items-center justify-between gap-3 rounded-xl border p-3"
            >
              <div className="min-w-0">
                <div className="text-sm font-medium">{KIND_LABELS[kind]}</div>
                {names.length > 0 ? (
                  <div className="text-muted-foreground truncate text-xs">{names.join(", ")}</div>
                ) : (
                  <div className="text-muted-foreground text-xs">Not set up</div>
                )}
              </div>
              {names.length === 0 && (
                <Button
                  variant="outline"
                  size="sm"
                  className="shrink-0"
                  onClick={() => onCreate(kind)}
                  disabled={isCreating}
                  aria-busy={isCreating}
                >
                  {isCreating ? (
                    <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" />
                  ) : (
                    <FolderPlus className="mr-1.5 h-3.5 w-3.5" />
                  )}
                  {isCreating ? "Creating..." : `Create ${spec.name}`}
                </Button>
              )}
            </div>
          );
        })}
      </CardContent>
    </Card>
  );
}

export default function AdminVirtualLibrary() {
  const [query, setQuery] = useState("");
  const [failedOnly, setFailedOnly] = useState(false);
  const [creating, setCreating] = useState<VirtualKind | null>(null);

  const libraries = useAdminLibraries();
  const createLibrary = useCreateLibrary();
  const { data: items = [], isLoading, error, isFetching, refetch } = useAdminVirtualItems();

  const coverage = useMemo(() => virtualCoverage(libraries.data), [libraries.data]);
  const showSetup =
    !libraries.isLoading &&
    libraries.data !== undefined &&
    (!coverage.hasMovies || !coverage.hasSeries);
  const librariesExist = (libraries.data?.length ?? 0) > 0;

  const filtered = useMemo(() => {
    const needle = query.trim().toLowerCase();
    return items.filter((item) => {
      if (failedOnly && item.failed_count <= 0) return false;
      if (!needle) return true;
      return item.title.toLowerCase().includes(needle) || String(item.id).toLowerCase().includes(needle);
    });
  }, [items, query, failedOnly]);

  function createVirtualLibrary(kind: VirtualKind) {
    setCreating(kind);
    createLibrary.mutate(CREATE_SPECS[kind], {
      onSettled: () => setCreating(null),
    });
  }

  return (
    <div className="page-shell space-y-6 py-4 sm:py-6">
      <div className="page-header gap-5">
        <div className="space-y-3">
          <h1 className="page-title text-[clamp(2rem,4vw,3rem)]">Virtual Library</h1>
          <p className="page-subtitle text-sm sm:text-base">
            Zero-storage catalog items registered by plugins. Nothing is stored on disk; items
            stream from their source on demand.
          </p>
        </div>
        <Button
          variant="outline"
          size="sm"
          className="min-w-[8.25rem] justify-center"
          onClick={() => void refetch()}
          disabled={isFetching}
          aria-busy={isFetching}
        >
          <RefreshCw className={cn("mr-1.5 h-3.5 w-3.5", isFetching && "animate-spin")} />
          {isFetching ? "Refreshing..." : "Refresh"}
        </Button>
      </div>

      {showSetup && (
        <SetupCard coverage={coverage} creating={creating} onCreate={createVirtualLibrary} />
      )}

      <Card className="surface-panel rounded-2xl border-0">
        <CardHeader className="gap-4 space-y-0">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <CardTitle className="text-sm font-bold">Release Queue</CardTitle>
            {!isLoading && !error && (
              <span className="text-muted-foreground text-xs">
                Showing {filtered.length} of {items.length}
              </span>
            )}
          </div>
          <div className="flex flex-wrap items-center gap-3">
            <div className="relative w-full sm:max-w-xs">
              <Search className="text-muted-foreground absolute top-1/2 left-3 h-4 w-4 -translate-y-1/2" />
              <Input
                value={query}
                onChange={(event) => setQuery(event.target.value)}
                placeholder="Filter by title or content id"
                aria-label="Filter virtual items"
                className="pl-9"
              />
            </div>
            <label
              htmlFor="virtual-failed-only"
              className="flex items-center gap-2 text-sm whitespace-nowrap"
            >
              <Switch
                id="virtual-failed-only"
                checked={failedOnly}
                onCheckedChange={setFailedOnly}
                aria-label="Failed only"
              />
              Failed only
            </label>
          </div>
        </CardHeader>
        <CardContent>
          {isLoading ? (
            <div className="space-y-3">
              <Skeleton className="h-10 w-full rounded-lg" />
              {Array.from({ length: 5 }).map((_, index) => (
                <Skeleton key={index} className="h-12 w-full rounded-lg" />
              ))}
            </div>
          ) : error ? (
            <div className="flex flex-col items-center justify-center gap-3 py-10 text-center">
              <AlertTriangle className="text-destructive h-8 w-8" />
              <p className="text-destructive text-sm">
                {error instanceof Error ? error.message : "Failed to load virtual items"}
              </p>
              <Button variant="outline" size="sm" onClick={() => void refetch()}>
                Try again
              </Button>
            </div>
          ) : items.length === 0 ? (
            librariesExist ? (
              <div className="flex flex-col items-center justify-center gap-3 py-12 text-center">
                <Inbox className="text-muted-foreground/50 h-10 w-10" />
                <div className="space-y-1">
                  <p className="text-sm font-medium">No virtual items yet</p>
                  <p className="text-muted-foreground max-w-sm text-xs">
                    Virtual items appear here once a virtual-library plugin registers them. Install
                    or configure one on the{" "}
                    <Link to="/admin/plugins" className="text-primary font-medium hover:underline">
                      Plugins page
                    </Link>
                    .
                  </p>
                </div>
              </div>
            ) : (
              <p className="text-muted-foreground py-8 text-center text-sm">
                Set up a virtual library above to get started.
              </p>
            )
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Title</TableHead>
                  <TableHead>Library</TableHead>
                  <TableHead>Plugin</TableHead>
                  <TableHead>Candidates</TableHead>
                  <TableHead>Last delivered</TableHead>
                  <TableHead>Last seen</TableHead>
                  <TableHead>Releases</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {filtered.map((item) => (
                  <TableRow key={item.id}>
                    <TableCell>
                      <div className="space-y-1">
                        <div className="flex items-center gap-2">
                          <span className="font-medium">{item.title || "Untitled"}</span>
                          <Badge variant="secondary">{typeLabel(item.type)}</Badge>
                        </div>
                        <div className="text-muted-foreground font-mono text-xs">{item.id}</div>
                      </div>
                    </TableCell>
                    <TableCell>
                      <div className="space-y-1">
                        <div>{item.library_name}</div>
                        <div className="text-muted-foreground font-mono text-xs">
                          #{item.library_id}
                        </div>
                      </div>
                    </TableCell>
                    <TableCell>{pluginLabel(String(item.installation_id))}</TableCell>
                    <TableCell>
                      <div className="flex items-center gap-2">
                        <span className="tabular-nums">{item.candidate_count}</span>
                        {item.failed_count > 0 && (
                          <Badge
                            variant="destructive"
                            title={`${item.failed_count} candidate deliveries failed`}
                          >
                            {item.failed_count} failed
                          </Badge>
                        )}
                      </div>
                    </TableCell>
                    <TableCell>
                      <RelativeCell value={item.last_delivered_at} />
                    </TableCell>
                    <TableCell>
                      <RelativeCell value={item.last_seen_at} />
                    </TableCell>
                    <TableCell>
                      <ReleaseCell releases={item.release_names} />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function ReleaseCell({ releases }: { releases: string[] }) {
  if (releases.length === 0) {
    return <span className="text-muted-foreground">—</span>;
  }
  return (
    <span title={releases.join(", ")}>
      {releases[0]}
      {releases.length > 1 ? ` +${releases.length - 1} more` : ""}
    </span>
  );
}
