# Episode layout aliases

The catalog can contain more than one episode row for the same provider episode
when a local library uses a different season layout than the metadata provider.
For example, a provider may number episodes by absolute season while the local
files use the provider's season and episode numbers. `episode_aliases` records
that duplicate layout row without deleting or renumbering either episode.

## Identity model

- `canonical_episode_id` is the episode the catalog displays.
- `alias_episode_id` is an optional concrete episode row that owns the same
  content under another layout. Alias rows are hidden from canonical catalog
  lists, but their files are available through the canonical episode.
- Provider coordinates (`provider_installation_id`, `provider_series_id`,
  `provider_season_number`, `provider_episode_number`) are an alternative
  lookup key for a provider episode that has no local row yet.
- A file's `episode_id` remains the row that owns the file. File-scoped work
  (playback authorization, markers, downloaded subtitles) keeps using it.
- Watch progress, history, resume, and scrobbles use the canonical episode.
  `PlaybackHandler` resolves an alias episode through the alias repository at
  that boundary. A legacy-data migration moves progress and history rows that
  were stored under alias IDs onto their canonical episode.

## Database invariants

The follow-up migration enforces:

- canonical and alias episodes belong to the alias row's `series_id`;
- a provider coordinate maps to at most one canonical episode per series;
- a canonical episode cannot also be an alias, and an alias row cannot become a
  canonical row, which prevents chains and cycles;
- a concrete alias episode maps to at most one canonical episode.

The repository also serializes alias writes per series and reports identity or
coordinate conflicts instead of choosing an arbitrary mapping.

## Activation and reconciliation

`episode_aliases` is additive and starts empty. Populate it with the maintenance
command:

```sh
silo episode-aliases reconcile <series-id> --dry-run
silo episode-aliases reconcile <series-id>
```

The reconciler locks one series, matches episode rows by exact title and air
date, and only proposes a mapping when exactly one fileless canonical row and
one or more file-backed layout rows exist. Ambiguous groups are reported and
left untouched. The dry run writes nothing and prints the candidate and skipped
groups for review.

## Rollout

Apply the migrations before starting binaries that contain the alias read paths;
the new queries reference `episode_aliases` unconditionally. Do not populate
mappings while old and new binaries are serving the same catalog: old binaries
do not know about aliases, so nodes can disagree about episode identity and
write incompatible progress. Upgrade every node, then run reconciliation.

The migration is additive and keeps both episode rows. Rolling back the binary
is safe. Dropping `episode_aliases` discards the mappings and breaks running
binaries that read them; do not run its `Down` migration while new binaries are
serving.
