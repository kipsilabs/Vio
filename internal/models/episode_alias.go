package models

import (
	"errors"
	"strings"
	"time"
)

// EpisodeAlias maps a provider/layout episode coordinate to the canonical
// episode the catalog should display. AliasEpisodeID is set when the alias is
// an existing episode row (for example a duplicate absolute-numbering row);
// provider aliases may have no local row and leave it nil.
type EpisodeAlias struct {
	ID                     int64
	SeriesID               string
	CanonicalEpisodeID     string
	AliasEpisodeID         *string
	ProviderInstallationID int64
	ProviderSeriesID       string
	ProviderSeasonNumber   int
	ProviderEpisodeNumber  int
	Source                 string
	Confidence             int
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// Validate checks the fields the alias repository relies on before a write.
func (a *EpisodeAlias) Validate() error {
	if a == nil {
		return errors.New("episode alias is nil")
	}
	if strings.TrimSpace(a.SeriesID) == "" {
		return errors.New("episode alias series_id is required")
	}
	if strings.TrimSpace(a.CanonicalEpisodeID) == "" {
		return errors.New("episode alias canonical_episode_id is required")
	}
	if a.AliasEpisodeID != nil {
		alias := strings.TrimSpace(*a.AliasEpisodeID)
		if alias == "" {
			return errors.New("episode alias alias_episode_id is empty")
		}
		if alias == a.CanonicalEpisodeID {
			return errors.New("episode alias cannot point at its canonical episode")
		}
	}
	if a.ProviderInstallationID < 0 {
		return errors.New("episode alias provider_installation_id is negative")
	}
	if a.ProviderSeasonNumber < 0 || a.ProviderEpisodeNumber < 0 {
		return errors.New("episode alias provider coordinates are negative")
	}
	if strings.TrimSpace(a.Source) == "" {
		return errors.New("episode alias source is required")
	}
	if a.Confidence < 0 || a.Confidence > 100 {
		return errors.New("episode alias confidence must be between 0 and 100")
	}
	return nil
}
