package catalog

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrReleaseOverrideForbidden = errors.New("release override access denied")
	ErrReleaseOverrideConflict  = errors.New("release override revision conflict")
	ErrInvalidReleaseOverride   = errors.New("invalid release override")
)

type ReleaseIdentity struct {
	MediaType     string `json:"media_type"`
	Provider      string `json:"provider"`
	ProviderID    string `json:"provider_id"`
	SeasonNumber  int    `json:"season_number"`
	EpisodeNumber int    `json:"episode_number"`
}

func (id ReleaseIdentity) Validate() error {
	if id.MediaType != "movie" && id.MediaType != "episode" {
		return fmt.Errorf("%w: media_type must be movie or episode", ErrInvalidReleaseOverride)
	}
	switch id.Provider {
	case "tmdb", "tvdb":
		if !numericProviderIDPattern.MatchString(id.ProviderID) {
			return fmt.Errorf("%w: noncanonical provider_id", ErrInvalidReleaseOverride)
		}
	case "imdb":
		if !imdbIDPattern.MatchString(id.ProviderID) {
			return fmt.Errorf("%w: noncanonical IMDb ID", ErrInvalidReleaseOverride)
		}
	default:
		return fmt.Errorf("%w: unsupported provider", ErrInvalidReleaseOverride)
	}
	if id.MediaType == "movie" {
		if id.Provider == "tvdb" || id.SeasonNumber != 0 || id.EpisodeNumber != 0 {
			return fmt.Errorf("%w: invalid movie identity", ErrInvalidReleaseOverride)
		}
	} else if id.SeasonNumber < 1 || id.SeasonNumber > 100000 || id.EpisodeNumber < 1 || id.EpisodeNumber > 100000 {
		return fmt.Errorf("%w: episode ordinals must be between 1 and 100000", ErrInvalidReleaseOverride)
	}
	return nil
}

func (id ReleaseIdentity) lockKey() string {
	return fmt.Sprintf("catalog:release:%s:%s:%s:%d:%d", id.MediaType, id.Provider, id.ProviderID, id.SeasonNumber, id.EpisodeNumber)
}

type ReleaseOverride struct {
	ReleaseIdentity
	Revision       int64      `json:"revision"`
	Action         string     `json:"action"`
	ReleaseAt      *time.Time `json:"release_at"`
	EvidenceNote   string     `json:"evidence_note"`
	ActorAccountID int        `json:"actor_account_id"`
	RecordedAt     time.Time  `json:"recorded_at"`
}

func (o ReleaseOverride) Decision(now time.Time) (released, overridden bool) {
	if o.ReleaseAt == nil {
		return false, false
	}
	return !o.ReleaseAt.After(now), true
}

type ReleaseOverrideMutation struct {
	ReleaseIdentity
	ExpectedRevision int64  `json:"expected_revision"`
	ReleaseAt        string `json:"release_at"`
	EvidenceNote     string `json:"evidence_note"`
}

func (m ReleaseOverrideMutation) validate(clear bool) (*time.Time, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if m.ExpectedRevision < 0 || m.ExpectedRevision == 1<<63-1 {
		return nil, fmt.Errorf("%w: invalid expected_revision", ErrInvalidReleaseOverride)
	}
	if !utf8.ValidString(m.EvidenceNote) || strings.TrimSpace(m.EvidenceNote) == "" || len(m.EvidenceNote) > 4096 || strings.ContainsRune(m.EvidenceNote, 0) {
		return nil, fmt.Errorf("%w: evidence_note is required and must not exceed 4096 bytes", ErrInvalidReleaseOverride)
	}
	if clear {
		if m.ReleaseAt != "" {
			return nil, fmt.Errorf("%w: clear must not include release_at", ErrInvalidReleaseOverride)
		}
		return nil, nil
	}
	layout := time.RFC3339Nano
	if len(m.ReleaseAt) == 10 {
		layout = time.DateOnly
	}
	date, err := time.Parse(layout, m.ReleaseAt)
	if err != nil || date.Year() < 1 || date.Year() > 9999 {
		return nil, fmt.Errorf("%w: release_at must be YYYY-MM-DD or RFC3339 with an offset", ErrInvalidReleaseOverride)
	}
	date = date.UTC()
	if date.Year() < 1 || date.Year() > 9999 {
		return nil, fmt.Errorf("%w: UTC release_at is out of range", ErrInvalidReleaseOverride)
	}
	date = date.Truncate(time.Microsecond)
	return &date, nil
}

type ReleaseOverrideLookup interface {
	Lookup(context.Context, ReleaseIdentity) (ReleaseOverride, error)
}

func (r *VirtualMediaRegistrar) LookupReleaseOverrides(ctx context.Context, ids []ReleaseIdentity) ([]ReleaseOverride, error) {
	if len(ids) > 100 {
		return nil, ErrInvalidReleaseOverride
	}
	batch := &pgx.Batch{}
	for _, id := range ids {
		if err := id.Validate(); err != nil {
			return nil, err
		}
		batch.Queue(releaseOverrideSelect+` ORDER BY revision DESC LIMIT 1`, id.MediaType, id.Provider, id.ProviderID, id.SeasonNumber, id.EpisodeNumber)
	}
	results := r.pool.SendBatch(ctx, batch)
	defer func() { _ = results.Close() }()
	out := make([]ReleaseOverride, 0, len(ids))
	for _, id := range ids {
		o, err := scanReleaseOverride(results.QueryRow(), id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		out = append(out, o)
	}
	return out, results.Close()
}

func (r *ReleaseOverrideRepository) Lookup(ctx context.Context, id ReleaseIdentity) (ReleaseOverride, error) {
	if err := id.Validate(); err != nil {
		return ReleaseOverride{}, err
	}
	o, err := scanReleaseOverride(r.pool.QueryRow(ctx, releaseOverrideSelect+` ORDER BY revision DESC LIMIT 1`, id.MediaType, id.Provider, id.ProviderID, id.SeasonNumber, id.EpisodeNumber), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReleaseOverride{ReleaseIdentity: id}, nil
	}
	return o, err
}

func releaseOverrideDecision(ctx context.Context, lookup ReleaseOverrideLookup, ids []ReleaseIdentity) (bool, bool, error) {
	if lookup == nil {
		return false, false, nil
	}
	released, overridden := true, false
	for _, id := range ids {
		o, err := lookup.Lookup(ctx, id)
		if err != nil {
			return false, false, err
		}
		if allowed, active := o.Decision(time.Now().UTC()); active {
			overridden = true
			released = released && allowed
		}
	}
	return released && overridden, overridden, nil
}

// decideReleaseOverrides folds captured override entries with the same
// latest-wins-per-alias, all-aliases-must-allow semantics as
// releaseOverrideDecision. It operates on already-read entries so callers
// holding a transaction can decide on the snapshot they validated.
func decideReleaseOverrides(now time.Time, entries []ReleaseOverride) (released, overridden bool) {
	released, overridden = true, false
	for _, o := range entries {
		if allowed, active := o.Decision(now); active {
			overridden = true
			released = released && allowed
		}
	}
	return released && overridden, overridden
}

// releaseAliasQuerier abstracts the read surface shared by *pgxpool.Pool and
// pgx.Tx so release-identity resolution works both pre-transaction and
// inside a transaction holding row locks.
type releaseAliasQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// releaseIdentitiesForContent unions incoming external IDs with the stored
// scalar and provider-table aliases for contentID. Override decisions must
// cover every known alias: a future override on any alias blocks, so a
// registration that omits a known alias cannot bypass it.
//
// Errors are propagated: an incomplete alias set must never be mistaken for
// a complete one. Only a missing catalog row degrades to incoming-only (there
// are no stored aliases yet). Callers holding the content lock (see
// lockReleaseContentTx) additionally know the set cannot change under them.
func releaseIdentitiesForContent(ctx context.Context, q releaseAliasQuerier, mediaType, contentID, itemType, tmdbID, tvdbID, imdbID string, season, episode int) ([]ReleaseIdentity, error) {
	ids := releaseIdentities(mediaType, tmdbID, tvdbID, imdbID, season, episode)
	if q == nil || contentID == "" {
		return sortReleaseIdentities(ids), nil
	}
	var storedTMDB, storedTVDB, storedIMDb string
	if err := q.QueryRow(ctx, `SELECT COALESCE(tmdb_id,''),COALESCE(tvdb_id,''),COALESCE(imdb_id,'') FROM media_items WHERE content_id=$1`, contentID).Scan(&storedTMDB, &storedTVDB, &storedIMDb); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("load stored release aliases: %w", err)
		}
	} else {
		ids = append(ids, releaseIdentities(mediaType, storedTMDB, storedTVDB, storedIMDb, season, episode)...)
	}
	rows, err := q.Query(ctx, `SELECT provider, provider_id FROM media_item_provider_ids WHERE content_id=$1 AND item_type=$2`, contentID, itemType)
	if err != nil {
		return nil, fmt.Errorf("list provider release aliases: %w", err)
	}
	for rows.Next() {
		var provider, providerID string
		if err := rows.Scan(&provider, &providerID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan provider release alias: %w", err)
		}
		id := ReleaseIdentity{MediaType: mediaType, Provider: provider, ProviderID: providerID, SeasonNumber: season, EpisodeNumber: episode}
		if id.Validate() == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate provider release aliases: %w", err)
	}
	return sortReleaseIdentities(ids), nil
}

func dedupeReleaseIdentities(ids []ReleaseIdentity) []ReleaseIdentity {
	seen := make(map[ReleaseIdentity]struct{}, len(ids))
	out := make([]ReleaseIdentity, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// releaseIdentitySet indexes identities for membership comparison.
func releaseIdentitySet(ids []ReleaseIdentity) map[ReleaseIdentity]struct{} {
	out := make(map[ReleaseIdentity]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

// releaseIdentitySetsEqual reports whether two identity sets contain exactly
// the same members, regardless of order or duplicates.
func releaseIdentitySetsEqual(a, b []ReleaseIdentity) bool {
	setA := releaseIdentitySet(a)
	if len(setA) != len(releaseIdentitySet(b)) {
		return false
	}
	for _, id := range b {
		if _, ok := setA[id]; !ok {
			return false
		}
	}
	return true
}

// releaseIdentitiesCovered reports whether every identity in subset is present
// in the superset.
func releaseIdentitiesCovered(superset, subset []ReleaseIdentity) bool {
	set := releaseIdentitySet(superset)
	for _, id := range subset {
		if _, ok := set[id]; !ok {
			return false
		}
	}
	return true
}

// recordedReleaseOverrides keeps only the entries a snapshot actually saw an
// override for. A never-recorded identity may gain one after preparation;
// collection acceptance still executes the pre-override decision, so it
// validates only the revisions it relied on.
func recordedReleaseOverrides(entries []ReleaseOverride) []ReleaseOverride {
	recorded := make([]ReleaseOverride, 0, len(entries))
	for _, entry := range entries {
		if entry.Revision != 0 {
			recorded = append(recorded, entry)
		}
	}
	return recorded
}

// snapshotIdentities extracts the identity set covered by captured entries.
func snapshotIdentities(entries []ReleaseOverride) []ReleaseIdentity {
	ids := make([]ReleaseIdentity, 0, len(entries))
	for _, o := range entries {
		ids = append(ids, o.ReleaseIdentity)
	}
	return ids
}

// sortReleaseIdentities orders identities deterministically so snapshots,
// lock acquisition, and tests are stable across runs.
func sortReleaseIdentities(ids []ReleaseIdentity) []ReleaseIdentity {
	out := dedupeReleaseIdentities(ids)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.MediaType != b.MediaType {
			return a.MediaType < b.MediaType
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.ProviderID != b.ProviderID {
			return a.ProviderID < b.ProviderID
		}
		if a.SeasonNumber != b.SeasonNumber {
			return a.SeasonNumber < b.SeasonNumber
		}
		return a.EpisodeNumber < b.EpisodeNumber
	})
	return out
}

// Release lock protocol (all locks transaction-scoped advisory locks).
//
// Alias-set stability uses one content-level key per catalog item:
//
//	catalog:release:content:<contentID>
//
// Readers (registration, collection preparation/acceptance, episode
// materialization) hold it SHARED from transaction start; alias writers
// (provider-ID attach/replace) hold it EXCLUSIVE from transaction start.
// Either order serializes: a writer committing before a reader is observed
// by that reader; a writer committing after is observed by the next
// decision. Per-identity shared/exclusive locks then serialize eligibility
// reads against override writes on the stabilized set.
//
// Global order inside a transaction: content lock, then collection/item
// locks, then per-identity locks, then refresh-debt row writes. Override
// mutation takes per-identity exclusive locks before its debt write, matching
// the readers' locks-before-debt order, so no debt/identity cycle can form.
func lockReleaseContentTx(ctx context.Context, tx pgx.Tx, contentID string, exclusive bool) error {
	return LockReleaseContent(ctx, tx, contentID, exclusive)
}

// LockReleaseContent serializes alias-set changes (exclusive) against
// release-eligibility reads (shared) for one content item. It must be the
// first lock in a transaction that writes aliases, and precede item-row and
// identity locks in readers; see the protocol note on
// releaseIdentitiesForContent. Callers inside another transaction must take
// it before acquiring any item-row, identity, or debt-row resource.
func LockReleaseContent(ctx context.Context, tx pgx.Tx, contentID string, exclusive bool) error {
	if tx == nil {
		return errors.New("transaction is required for release locking")
	}
	if contentID == "" {
		return errors.New("content ID is required for release locking")
	}
	key := "catalog:release:content:" + contentID
	if exclusive {
		_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key)
		return err
	}
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended($1,0))`, key)
	return err
}

// captureReleaseOverridesTx reads the latest revision per identity inside the
// caller's transaction, after row/advisory locks are held. Identities are
// read in one batch round trip rather than serial queries.
func captureReleaseOverridesTx(ctx context.Context, tx pgx.Tx, ids []ReleaseIdentity) ([]ReleaseOverride, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	batch := &pgx.Batch{}
	for _, id := range ids {
		batch.Queue(releaseOverrideSelect+` ORDER BY revision DESC LIMIT 1`, id.MediaType, id.Provider, id.ProviderID, id.SeasonNumber, id.EpisodeNumber)
	}
	results := tx.SendBatch(ctx, batch)
	defer func() { _ = results.Close() }()
	out := make([]ReleaseOverride, 0, len(ids))
	for _, id := range ids {
		o, err := scanReleaseOverride(results.QueryRow(), id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		o.ReleaseIdentity = id
		out = append(out, o)
	}
	return out, results.Close()
}

// hasPhysicalMediaFiles reports whether the catalog holds a non-virtual file
// for contentID. Callers use it as release evidence of last resort: with no
// active override, possession proves a release even when provider data is
// missing, negative, or misdated.
func hasPhysicalMediaFiles(ctx context.Context, q releaseAliasQuerier, contentID string) (bool, error) {
	if q == nil || contentID == "" {
		return false, nil
	}
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM media_files WHERE content_id=$1 AND container<>'virtual' AND file_path NOT LIKE 'virtual://%')`, contentID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check physical media files: %w", err)
	}
	return exists, nil
}

// lockReleaseIdentitiesTx takes shared transaction-scoped advisory locks on
// every identity so a concurrent override write (exclusive lock) serializes
// against this transaction's eligibility read. Keys are acquired in sorted
// order for a deterministic global sequence, batched in a single round-trip.
func lockReleaseIdentitiesTx(ctx context.Context, tx pgx.Tx, ids []ReleaseIdentity) error {
	deduped := dedupeReleaseIdentities(ids)
	if len(deduped) == 0 {
		return nil
	}
	sorted := sortReleaseIdentities(deduped)
	keys := make([]string, 0, len(sorted))
	for _, id := range sorted {
		keys = append(keys, id.lockKey())
	}
	_, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock_shared(hashtextextended(k, 0))
		FROM unnest($1::text[]) WITH ORDINALITY AS t(k, ord)
		ORDER BY ord`, keys)
	return err
}

func releaseIdentities(mediaType, tmdbID, tvdbID, imdbID string, season, episode int) []ReleaseIdentity {
	ids := make([]ReleaseIdentity, 0, 3)
	for _, pair := range [][2]string{{"tmdb", strings.TrimSpace(tmdbID)}, {"tvdb", strings.TrimSpace(tvdbID)}, {"imdb", strings.TrimSpace(imdbID)}} {
		if pair[1] == "" {
			continue
		}
		id := ReleaseIdentity{MediaType: mediaType, Provider: pair[0], ProviderID: pair[1], SeasonNumber: season, EpisodeNumber: episode}
		if id.Validate() == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func captureReleaseOverrides(ctx context.Context, lookup ReleaseOverrideLookup, ids []ReleaseIdentity) ([]ReleaseOverride, error) {
	if lookup == nil {
		return nil, nil
	}
	out := make([]ReleaseOverride, 0, len(ids))
	for _, id := range ids {
		o, err := lookup.Lookup(ctx, id)
		if err != nil {
			return nil, err
		}
		o.ReleaseIdentity = id
		out = append(out, o)
	}
	return out, nil
}

func validateReleaseSnapshotTx(ctx context.Context, tx pgx.Tx, snapshot []ReleaseOverride) error {
	if len(snapshot) == 0 {
		return nil
	}
	ids := snapshotIdentities(snapshot)
	if err := lockReleaseIdentitiesTx(ctx, tx, ids); err != nil {
		return err
	}
	currents, err := captureReleaseOverridesTx(ctx, tx, ids)
	if err != nil {
		return err
	}
	currentBySeq := make(map[ReleaseIdentity]int64, len(currents))
	for _, cur := range currents {
		currentBySeq[cur.ReleaseIdentity] = cur.Revision
	}
	for _, expected := range snapshot {
		// Absence must be explicit: a missing key means no override row
		// exists, which only matches a zero-revision (never-recorded)
		// expectation. A recorded revision can never be zero.
		current, ok := currentBySeq[expected.ReleaseIdentity]
		if !ok {
			if expected.Revision != 0 {
				return ErrReleaseOverrideConflict
			}
			continue
		}
		if current != expected.Revision {
			return ErrReleaseOverrideConflict
		}
	}
	return nil
}

type ReleaseOverrideRepository struct {
	pool *pgxpool.Pool
}

func NewReleaseOverrideRepository(pool *pgxpool.Pool) *ReleaseOverrideRepository {
	return &ReleaseOverrideRepository{pool: pool}
}

func requireReleaseOverrideAdmin(ctx context.Context, tx pgx.Tx, actor int) error {
	if actor <= 0 {
		return ErrReleaseOverrideForbidden
	}
	var authorized bool
	err := tx.QueryRow(ctx, `SELECT role='admin' AND enabled IS TRUE FROM users WHERE id=$1 FOR SHARE`, actor).Scan(&authorized)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !authorized {
		return ErrReleaseOverrideForbidden
	}
	return err
}

const releaseOverrideSelect = `SELECT revision,action,release_at,evidence_note,actor_account_id,recorded_at
FROM verified_release_override_history
WHERE media_type=$1 AND provider=$2 AND provider_id=$3 AND season_number=$4 AND episode_number=$5`

func scanReleaseOverride(row pgx.Row, id ReleaseIdentity) (ReleaseOverride, error) {
	o := ReleaseOverride{ReleaseIdentity: id}
	err := row.Scan(&o.Revision, &o.Action, &o.ReleaseAt, &o.EvidenceNote, &o.ActorAccountID, &o.RecordedAt)
	if o.ReleaseAt != nil {
		date := o.ReleaseAt.UTC()
		o.ReleaseAt = &date
	}
	o.RecordedAt = o.RecordedAt.UTC()
	return o, err
}

func (r *ReleaseOverrideRepository) Mutate(ctx context.Context, actor int, m ReleaseOverrideMutation, clear bool) (ReleaseOverride, error) {
	var empty ReleaseOverride
	if actor <= 0 {
		return empty, ErrReleaseOverrideForbidden
	}
	date, err := m.validate(clear)
	if err != nil {
		return empty, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := requireReleaseOverrideAdmin(ctx, tx, actor); err != nil {
		return empty, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, m.lockKey()); err != nil {
		return empty, err
	}
	current, err := scanReleaseOverride(tx.QueryRow(ctx, releaseOverrideSelect+` ORDER BY revision DESC LIMIT 1`, m.MediaType, m.Provider, m.ProviderID, m.SeasonNumber, m.EpisodeNumber), m.ReleaseIdentity)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	if current.Revision != m.ExpectedRevision || clear && current.ReleaseAt == nil {
		return empty, ErrReleaseOverrideConflict
	}
	action := "set"
	if clear {
		action = "clear"
	} else if current.ReleaseAt != nil {
		action = "change"
	}
	o, err := scanReleaseOverride(tx.QueryRow(ctx, `INSERT INTO verified_release_override_history
(media_type,provider,provider_id,season_number,episode_number,revision,action,release_at,evidence_note,actor_account_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
RETURNING revision,action,release_at,evidence_note,actor_account_id,recorded_at`, m.MediaType, m.Provider, m.ProviderID, m.SeasonNumber, m.EpisodeNumber, current.Revision+1, action, date, strings.TrimSpace(m.EvidenceNote), actor), m.ReleaseIdentity)
	if err != nil {
		return empty, err
	}
	if _, err := enqueueReleaseMetadataRetry(ctx, tx, m.ReleaseIdentity); err != nil {
		return empty, err
	}
	if err := tx.Commit(ctx); err != nil {
		return empty, err
	}
	return o, nil
}

func (r *ReleaseOverrideRepository) Read(ctx context.Context, actor int, id ReleaseIdentity, before int64, limit int) ([]ReleaseOverride, error) {
	if actor <= 0 {
		return nil, ErrReleaseOverrideForbidden
	}
	if err := id.Validate(); err != nil {
		return nil, err
	}
	if before < 0 || limit < 1 || limit > 100 {
		return nil, fmt.Errorf("%w: invalid history pagination", ErrInvalidReleaseOverride)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := requireReleaseOverrideAdmin(ctx, tx, actor); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, releaseOverrideSelect+` AND ($6::bigint=0 OR revision<$6) ORDER BY revision DESC LIMIT $7`, id.MediaType, id.Provider, id.ProviderID, id.SeasonNumber, id.EpisodeNumber, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ReleaseOverride, 0)
	for rows.Next() {
		o, err := scanReleaseOverride(rows, id)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit(ctx)
}
