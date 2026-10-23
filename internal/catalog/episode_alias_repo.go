package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/models"
)

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

// Upsert inserts or updates one mapping. The canonical/provider tuple is the
// write identity; a concrete alias row remains unique across the series.
func (r *EpisodeAliasRepository) Upsert(ctx context.Context, alias *models.EpisodeAlias) error {
	if r == nil || r.pool == nil {
		return errors.New("episode alias repository is not configured")
	}
	if err := alias.Validate(); err != nil {
		return err
	}
	var aliasEpisode any
	if alias.AliasEpisodeID != nil {
		aliasEpisode = *alias.AliasEpisodeID
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO episode_aliases (
			series_id, canonical_episode_id, alias_episode_id,
			provider_installation_id, provider_series_id,
			provider_season_number, provider_episode_number,
			source, confidence
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (
			canonical_episode_id,
			provider_installation_id,
			provider_series_id,
			provider_season_number,
			provider_episode_number
		) DO UPDATE SET
			series_id = EXCLUDED.series_id,
			alias_episode_id = EXCLUDED.alias_episode_id,
			source = EXCLUDED.source,
			confidence = EXCLUDED.confidence,
			updated_at = now()`,
		alias.SeriesID,
		alias.CanonicalEpisodeID,
		aliasEpisode,
		alias.ProviderInstallationID,
		alias.ProviderSeriesID,
		alias.ProviderSeasonNumber,
		alias.ProviderEpisodeNumber,
		alias.Source,
		alias.Confidence,
	)
	if err != nil {
		return fmt.Errorf("upserting episode alias: %w", err)
	}
	return nil
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
		  AND provider_episode_number = $5
		LIMIT 1`,
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
