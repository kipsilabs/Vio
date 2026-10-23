package catalog

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/models"
)

// ErrEpisodeAliasConflict reports an alias write that would violate the map's
// identity or coordinate rules. The caller can surface the message and keep the
// existing mapping rather than treating it as a transient database failure.
var ErrEpisodeAliasConflict = errors.New("episode alias conflicts with an existing mapping")

// EpisodeAliasRepository owns the canonical-episode alias map. The map is
// additive: it never renumbers or deletes episode rows itself.
type EpisodeAliasRepository struct {
	pool *pgxpool.Pool
}

func NewEpisodeAliasRepository(pool *pgxpool.Pool) *EpisodeAliasRepository {
	return &EpisodeAliasRepository{pool: pool}
}

const episodeAliasColumns = `id, series_id, canonical_episode_id, alias_episode_id,
	provider_installation_id, provider_series_id, provider_season_number,
	provider_episode_number, source, confidence, created_at, updated_at`

// Upsert inserts or updates one mapping. The series/provider coordinate is the
// write identity; a concrete alias row remains unique across the series. The
// write is serialized per series so two reconcilers cannot create a chain or
// cycle between the same pair of episode rows.
func (r *EpisodeAliasRepository) Upsert(ctx context.Context, alias *models.EpisodeAlias) error {
	if r == nil || r.pool == nil {
		return errors.New("episode alias repository is not configured")
	}
	if err := alias.Validate(); err != nil {
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning episode alias transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	if err := lockEpisodeAliasSeries(ctx, tx, alias.SeriesID); err != nil {
		return err
	}
	if err := upsertEpisodeAlias(ctx, tx, alias); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing episode alias: %w", err)
	}
	return nil
}

func lockEpisodeAliasSeries(ctx context.Context, tx pgx.Tx, seriesID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, seriesID); err != nil {
		return fmt.Errorf("locking episode alias series: %w", err)
	}
	return nil
}

func upsertEpisodeAlias(ctx context.Context, tx pgx.Tx, alias *models.EpisodeAlias) error {
	var aliasEpisode any
	if alias.AliasEpisodeID != nil {
		aliasEpisode = *alias.AliasEpisodeID
	}
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO episode_aliases (
			series_id, canonical_episode_id, alias_episode_id,
			provider_installation_id, provider_series_id,
			provider_season_number, provider_episode_number,
			source, confidence
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (
			series_id,
			provider_installation_id,
			provider_series_id,
			provider_season_number,
			provider_episode_number
		) DO UPDATE SET
			alias_episode_id = EXCLUDED.alias_episode_id,
			source = EXCLUDED.source,
			confidence = EXCLUDED.confidence,
			updated_at = now()
		WHERE episode_aliases.canonical_episode_id = EXCLUDED.canonical_episode_id
		RETURNING id`,
		alias.SeriesID,
		alias.CanonicalEpisodeID,
		aliasEpisode,
		alias.ProviderInstallationID,
		alias.ProviderSeriesID,
		alias.ProviderSeasonNumber,
		alias.ProviderEpisodeNumber,
		alias.Source,
		alias.Confidence,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: provider coordinate is already assigned to another canonical episode", ErrEpisodeAliasConflict)
	}
	if err != nil {
		return classifyEpisodeAliasWriteError(err)
	}
	return nil
}

func classifyEpisodeAliasWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503", "23505", "23514":
			return fmt.Errorf("%w: %s", ErrEpisodeAliasConflict, pgErr.Message)
		}
	}
	return fmt.Errorf("upserting episode alias: %w", err)
}

// ListBySeries returns the alias map for a series, including local-layout and
// provider rows, ordered by canonical episode id and provider coordinates.
func (r *EpisodeAliasRepository) ListBySeries(ctx context.Context, seriesID string) ([]*models.EpisodeAlias, error) {
	if r == nil || r.pool == nil {
		return nil, errors.New("episode alias repository is not configured")
	}
	if strings.TrimSpace(seriesID) == "" {
		return nil, errors.New("episode alias series_id is required")
	}
	rows, err := r.pool.Query(ctx, `
		SELECT `+episodeAliasColumns+`
		FROM episode_aliases
		WHERE series_id = $1
		ORDER BY canonical_episode_id, provider_installation_id,
			provider_series_id, provider_season_number, provider_episode_number`,
		seriesID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing episode aliases: %w", err)
	}
	defer rows.Close()

	aliases := make([]*models.EpisodeAlias, 0)
	for rows.Next() {
		alias, scanErr := scanEpisodeAlias(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		aliases = append(aliases, alias)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating episode aliases: %w", err)
	}
	return aliases, nil
}

// EpisodeAliasReconcileCandidate is one safe title/air-date match: a fileless
// canonical episode and a file-backed layout row that can carry its files.
type EpisodeAliasReconcileCandidate struct {
	CanonicalEpisodeID     string
	AliasEpisodeID         string
	CanonicalSeasonNumber  int
	CanonicalEpisodeNumber int
	AliasSeasonNumber      int
	AliasEpisodeNumber     int
	Title                  string
	AirDate                time.Time
}

// EpisodeAliasReconcileSkip records a duplicate group the reconciler refused to
// guess about. The operator can inspect these before applying anything.
type EpisodeAliasReconcileSkip struct {
	Title      string
	AirDate    time.Time
	EpisodeIDs []string
	Reason     string
}

// EpisodeAliasReconcileResult is the auditable outcome of one series
// reconciliation. Candidates are returned for both dry runs and applies.
type EpisodeAliasReconcileResult struct {
	SeriesID   string
	DryRun     bool
	Candidates []EpisodeAliasReconcileCandidate
	Skipped    []EpisodeAliasReconcileSkip
	Applied    int
}

// ReconcileSeries proposes and optionally applies one-hop layout aliases for a
// series. It matches canonical metadata episodes to duplicate layout rows by
// exact title and air date, requiring the canonical row to have no present file
// and the alias row to have one. Ambiguous groups are reported, never guessed.
//
// The whole series is locked for the transaction, so a concurrent apply cannot
// create a chain or cycle between the same episode rows.
func (r *EpisodeAliasRepository) ReconcileSeries(ctx context.Context, seriesID string, dryRun bool) (*EpisodeAliasReconcileResult, error) {
	if r == nil || r.pool == nil {
		return nil, errors.New("episode alias repository is not configured")
	}
	seriesID = strings.TrimSpace(seriesID)
	if seriesID == "" {
		return nil, errors.New("episode alias series_id is required")
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning episode alias reconciliation: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	if err := lockEpisodeAliasSeries(ctx, tx, seriesID); err != nil {
		return nil, err
	}
	grouped, err := loadReconcileEpisodes(ctx, tx, seriesID)
	if err != nil {
		return nil, err
	}
	result := buildReconcileResult(seriesID, dryRun, grouped)
	if dryRun {
		return result, nil
	}
	if err := applyReconcileCandidates(ctx, tx, seriesID, result.Candidates); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing episode alias reconciliation: %w", err)
	}
	result.Applied = len(result.Candidates)
	return result, nil
}

func loadReconcileEpisodes(ctx context.Context, tx pgx.Tx, seriesID string) (map[string][]reconcileEpisodeRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT e.content_id, e.season_number, e.episode_number,
		       COALESCE(e.title, ''), e.air_date,
		       EXISTS (
		           SELECT 1
		           FROM media_files mf
		           WHERE mf.episode_id = e.content_id
		             AND mf.missing_since IS NULL
		       ) AS has_file
		FROM episodes e
		WHERE e.series_id = $1
		  AND e.air_date IS NOT NULL
		  AND btrim(COALESCE(e.title, '')) <> ''
		ORDER BY e.air_date, lower(btrim(e.title)), e.season_number,
		         e.episode_number, e.content_id`, seriesID)
	if err != nil {
		return nil, fmt.Errorf("listing episodes for alias reconciliation: %w", err)
	}
	defer rows.Close()

	grouped := make(map[string][]reconcileEpisodeRow)
	for rows.Next() {
		var row reconcileEpisodeRow
		if err := rows.Scan(&row.contentID, &row.season, &row.episode, &row.title, &row.airDate, &row.hasFile); err != nil {
			return nil, fmt.Errorf("scanning episode for alias reconciliation: %w", err)
		}
		key := strings.ToLower(strings.TrimSpace(row.title)) + "\x00" + row.airDate.Format("2006-01-02")
		grouped[key] = append(grouped[key], row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating episodes for alias reconciliation: %w", err)
	}
	return grouped, nil
}

func buildReconcileResult(seriesID string, dryRun bool, grouped map[string][]reconcileEpisodeRow) *EpisodeAliasReconcileResult {
	result := &EpisodeAliasReconcileResult{SeriesID: seriesID, DryRun: dryRun}
	for _, group := range grouped {
		if len(group) < 2 {
			continue
		}
		canonical, aliases, skip := chooseReconcileAliases(group)
		if skip != nil {
			result.Skipped = append(result.Skipped, *skip)
			continue
		}
		for _, alias := range aliases {
			result.Candidates = append(result.Candidates, EpisodeAliasReconcileCandidate{
				CanonicalEpisodeID:     canonical.contentID,
				AliasEpisodeID:         alias.contentID,
				CanonicalSeasonNumber:  canonical.season,
				CanonicalEpisodeNumber: canonical.episode,
				AliasSeasonNumber:      alias.season,
				AliasEpisodeNumber:     alias.episode,
				Title:                  canonical.title,
				AirDate:                canonical.airDate,
			})
		}
	}
	sort.Slice(result.Candidates, func(i, j int) bool {
		left, right := result.Candidates[i], result.Candidates[j]
		if !left.AirDate.Equal(right.AirDate) {
			return left.AirDate.Before(right.AirDate)
		}
		if left.Title != right.Title {
			return left.Title < right.Title
		}
		if left.CanonicalEpisodeID != right.CanonicalEpisodeID {
			return left.CanonicalEpisodeID < right.CanonicalEpisodeID
		}
		return left.AliasEpisodeID < right.AliasEpisodeID
	})
	sort.Slice(result.Skipped, func(i, j int) bool {
		left, right := result.Skipped[i], result.Skipped[j]
		if !left.AirDate.Equal(right.AirDate) {
			return left.AirDate.Before(right.AirDate)
		}
		return left.Title < right.Title
	})
	return result
}

func applyReconcileCandidates(ctx context.Context, tx pgx.Tx, seriesID string, candidates []EpisodeAliasReconcileCandidate) error {
	for _, candidate := range candidates {
		aliasEpisodeID := candidate.AliasEpisodeID
		alias := &models.EpisodeAlias{
			SeriesID:              seriesID,
			CanonicalEpisodeID:    candidate.CanonicalEpisodeID,
			AliasEpisodeID:        &aliasEpisodeID,
			ProviderSeasonNumber:  candidate.AliasSeasonNumber,
			ProviderEpisodeNumber: candidate.AliasEpisodeNumber,
			Source:                "title_air_date",
			Confidence:            100,
		}
		if err := upsertEpisodeAlias(ctx, tx, alias); err != nil {
			return fmt.Errorf("applying episode alias %s -> %s: %w", candidate.CanonicalEpisodeID, candidate.AliasEpisodeID, err)
		}
	}
	return nil
}

type reconcileEpisodeRow struct {
	contentID string
	season    int
	episode   int
	title     string
	airDate   time.Time
	hasFile   bool
}

func chooseReconcileAliases(group []reconcileEpisodeRow) (reconcileEpisodeRow, []reconcileEpisodeRow, *EpisodeAliasReconcileSkip) {
	var canonical []reconcileEpisodeRow
	var aliases []reconcileEpisodeRow
	for _, row := range group {
		if row.hasFile {
			aliases = append(aliases, row)
		} else {
			canonical = append(canonical, row)
		}
	}
	skip := func(reason string) (reconcileEpisodeRow, []reconcileEpisodeRow, *EpisodeAliasReconcileSkip) {
		ids := make([]string, 0, len(group))
		for _, row := range group {
			ids = append(ids, row.contentID)
		}
		return reconcileEpisodeRow{}, nil, &EpisodeAliasReconcileSkip{
			Title: group[0].title, AirDate: group[0].airDate, EpisodeIDs: ids, Reason: reason,
		}
	}
	switch {
	case len(canonical) == 0:
		return skip("no fileless canonical episode")
	case len(canonical) > 1:
		return skip("multiple fileless canonical episodes")
	case len(aliases) == 0:
		return skip("no file-backed alias episode")
	}
	canonicalRow := canonical[0]
	seenCoordinates := make(map[[2]int]string, len(aliases))
	uniqueAliases := make([]reconcileEpisodeRow, 0, len(aliases))
	for _, alias := range aliases {
		if alias.season == canonicalRow.season && alias.episode == canonicalRow.episode {
			return skip("canonical and alias share a season/episode coordinate")
		}
		coordinate := [2]int{alias.season, alias.episode}
		if previous, ok := seenCoordinates[coordinate]; ok && previous != alias.contentID {
			return skip("multiple episode rows share the alias coordinate")
		}
		seenCoordinates[coordinate] = alias.contentID
		uniqueAliases = append(uniqueAliases, alias)
	}
	return canonicalRow, uniqueAliases, nil
}

// ResolveCanonicalEpisodeIDByAlias returns the canonical episode for a
// concrete alias row. found=false is a normal unmapped episode, not an error.
// The partial unique index on alias_episode_id makes this one indexed lookup.
func (r *EpisodeAliasRepository) ResolveCanonicalEpisodeIDByAlias(ctx context.Context, aliasEpisodeID string) (string, bool, error) {
	if r == nil || r.pool == nil {
		return "", false, errors.New("episode alias repository is not configured")
	}
	aliasEpisodeID = strings.TrimSpace(aliasEpisodeID)
	if aliasEpisodeID == "" {
		return "", false, nil
	}
	var canonicalID string
	err := r.pool.QueryRow(ctx, `
		SELECT canonical_episode_id
		FROM episode_aliases
		WHERE alias_episode_id = $1`, aliasEpisodeID).Scan(&canonicalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolving canonical alias episode: %w", err)
	}
	return canonicalID, true, nil
}

// ResolveCanonicalEpisodeID returns the canonical episode for a provider
// coordinate. found=false is a normal unmapped candidate, not an error.
func (r *EpisodeAliasRepository) ResolveCanonicalEpisodeID(
	ctx context.Context,
	seriesID string,
	providerInstallationID int64,
	providerSeriesID string,
	providerSeasonNumber int,
	providerEpisodeNumber int,
) (string, bool, error) {
	if r == nil || r.pool == nil {
		return "", false, errors.New("episode alias repository is not configured")
	}
	if strings.TrimSpace(seriesID) == "" {
		return "", false, errors.New("episode alias series_id is required")
	}
	var canonicalID string
	err := r.pool.QueryRow(ctx, `
		SELECT canonical_episode_id
		FROM episode_aliases
		WHERE series_id = $1
		  AND provider_installation_id = $2
		  AND provider_series_id = $3
		  AND provider_season_number = $4
		  AND provider_episode_number = $5`,
		seriesID, providerInstallationID, providerSeriesID,
		providerSeasonNumber, providerEpisodeNumber,
	).Scan(&canonicalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolving episode alias: %w", err)
	}
	return canonicalID, true, nil
}

func scanEpisodeAlias(row groupRowScanner) (*models.EpisodeAlias, error) {
	var alias models.EpisodeAlias
	var aliasEpisode *string
	if err := row.Scan(
		&alias.ID,
		&alias.SeriesID,
		&alias.CanonicalEpisodeID,
		&aliasEpisode,
		&alias.ProviderInstallationID,
		&alias.ProviderSeriesID,
		&alias.ProviderSeasonNumber,
		&alias.ProviderEpisodeNumber,
		&alias.Source,
		&alias.Confidence,
		&alias.CreatedAt,
		&alias.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("scanning episode alias: %w", err)
	}
	alias.AliasEpisodeID = aliasEpisode
	return &alias, nil
}
