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

## Watch-state writes

Every watch-state write resolves the submitted id to its canonical episode
before it writes, at the playback boundary and again in storage. A failed
resolution never falls back to the submitted alias id: the write fails instead
of recreating the hidden row that canonical reads ignore, and the caller can
retry with the id it already holds. There is no single wire code for that
failure: v2 bulk progress sync marks the item `dependency_unavailable`, v1 bulk
sync returns a per-item `error`, and best-effort playback persistence only logs
it. Treat the failed write as retryable rather than keying off one code.

Resolution and the row write are serialized with reconciliation by one
per-series advisory lock. A storage write takes its shared form
(`pg_advisory_xact_lock_shared`) before resolving the id and holds it through
commit; reconciliation and the alias identity trigger take it exclusively. A
write therefore either sees the mapping reconciliation already committed, or
commits before reconciliation takes its snapshot and repairs the row it wrote.
Batch writes collect their series and lock them in id order, so two writers
spanning several series cannot deadlock against each other.

## Database invariants

The follow-up migration enforces:

- canonical and alias episodes belong to the alias row's `series_id`;
- a provider coordinate maps to at most one canonical episode per series;
- a canonical episode cannot also be an alias, and an alias row cannot become a
  canonical row, which prevents chains and cycles;
- a concrete alias episode maps to at most one canonical episode.

The repository also serializes alias writes per series and reports identity or
coordinate conflicts instead of choosing an arbitrary mapping.

The canonicalizing repair runs the same chain/cycle check before it moves any
watch state, in the migration's transaction. A malformed install therefore
fails the migration with progress, history, and alias rows unchanged instead of
merging state onto an intermediate row and deleting it.

## Activation and reconciliation

`episode_aliases` is additive and starts empty. Populate it with the maintenance
command:

```sh
silo episode-aliases reconcile <series-id> --dry-run
silo episode-aliases reconcile <series-id>
```

Flags may precede or follow the series ID; `-env <file>` selects the bootstrap
file in either position. The reconciler locks one series, matches episode rows
by exact title and air date, and only proposes a mapping when exactly one
fileless canonical row and one or more file-backed layout rows exist. Ambiguous
groups are reported and left untouched. The dry run writes nothing and prints
the candidate and skipped groups for review.

## Rollout

Apply the migrations before starting binaries that contain the alias read paths;
the new queries reference `episode_aliases` unconditionally. Do not populate
mappings while old and new binaries are serving the same catalog: old binaries
do not know about aliases, so nodes can disagree about episode identity and
write incompatible progress. Upgrade every node, then run reconciliation.

The migration is additive and keeps both episode rows. Rolling back the binary
is safe while `episode_aliases` is empty. Once aliases are populated, an old
binary still starts and ignores the table, but it serves the alias-unaware
episode identity, so it no longer agrees with new binaries about which episode
owns progress and files. Dropping `episode_aliases`, or running its `Down`
migration, discards the mappings, hides the identity link, and breaks running
binaries that read the table; do not run `Down` while new binaries are serving.
