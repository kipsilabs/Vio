package api

import (
	"context"
	"net/url"
	"strings"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/quality"
)

// virtualCandidateLister is the slice of *virtuallibrary.Service the score
// adapter uses. It is an interface so the mapping is testable without a live
// resolver.
type virtualCandidateLister interface {
	ListStreams(ctx context.Context, virtualPath string) ([]virtuallibrary.PlaybackStream, error)
}

// virtualCandidateScoreSource adapts a virtual library's ranked candidate list
// to the catalog's score port, so a watch response can show each virtual
// version's custom-format score. It lives on the api side because the virtual
// library service imports catalog, so catalog cannot import it back.
//
// Scores come from the same ranking the playback picker uses: ListStreams
// re-lists (usually from the resolver's bounded candidate cache) and applies
// the active profile's custom formats. A provider miss returns no scores rather
// than an error, so the watch response still serves.
func virtualCandidateScoreSource(service virtualCandidateLister) catalog.VirtualCandidateScoreSource {
	if service == nil {
		return nil
	}
	return func(ctx context.Context, _ string, files []*models.MediaFile) map[int]catalog.VirtualCandidateScore {
		neutral := virtualNeutralSourcePath(files)
		if neutral == "" {
			return nil
		}
		streams, err := service.ListStreams(ctx, neutral)
		if err != nil || len(streams) == 0 {
			return nil
		}
		byURI := make(map[string]catalog.VirtualCandidateScore, len(streams))
		providerOrder := make(map[string]*int)
		for i, stream := range streams {
			// Zero is the unscored/neutral sum; the wire fields are omitted
			// for it. Provider-order positions still travel through this port
			// without becoming format scores on the wire.
			if stream.QualityScore != 0 || stream.PreserveProviderOrder {
				score := catalog.VirtualCandidateScore{
					Score:          stream.QualityScore,
					MatchedFormats: stream.MatchedFormats,
				}
				if stream.PreserveProviderOrder {
					score.ProviderOrder = &i
					providerOrder[virtualProviderOrderKey(stream.URI)] = &i
				}
				byURI[stream.URI] = score
			}
		}
		if len(byURI) == 0 {
			return nil
		}
		scores := make(map[int]catalog.VirtualCandidateScore, len(files))
		for _, file := range files {
			if file == nil {
				continue
			}
			if score, ok := byURI[file.FilePath]; ok {
				scores[file.ID] = score
			} else if position, ok := providerOrder[virtualProviderOrderKey(file.FilePath)]; ok {
				scores[file.ID] = catalog.VirtualCandidateScore{ProviderOrder: position}
			}
		}
		if len(scores) == 0 {
			return nil
		}
		return scores
	}
}

// Profile views share the same provider response and candidate IDs. Removing
// only the profile selector lets every view retain that provider order.
func virtualProviderOrderKey(rawURI string) string {
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return rawURI
	}
	query := parsed.Query()
	query.Del("profile")
	query.Del("results")
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// virtualRankingResolver is the slice of *virtuallibrary.Service the ranking
// adapter uses. It resolves the ranking from configuration alone, so no
// provider round-trip is needed.
type virtualRankingResolver interface {
	RankingForPath(virtualPath string) virtuallibrary.VirtualRanking
}

// virtualCandidateRankingSource adapts the virtual library's per-listing
// ranking to the catalog's ranking port. A listing's ranking is derived from
// the candidate URIs' profile selector, so every file of one listing shares it
// and the projection stays profile-scoped per variant row. Local files and
// non-virtual paths yield no entry.
func virtualCandidateRankingSource(service virtualRankingResolver) catalog.VirtualRankingSource {
	if service == nil {
		return nil
	}
	return func(_ context.Context, _ string, files []*models.MediaFile) map[int]catalog.VirtualRanking {
		ranked := make(map[int]catalog.VirtualRanking, len(files))
		byListing := make(map[string]catalog.VirtualRanking)
		for _, file := range files {
			if file == nil || !strings.HasPrefix(file.FilePath, "virtual://") {
				continue
			}
			neutral := virtualNeutralSourcePath([]*models.MediaFile{file})
			if neutral == "" {
				continue
			}
			ranking, ok := byListing[neutral]
			if !ok {
				view := service.RankingForPath(neutral)
				ranking = catalog.VirtualRanking{
					ProfileLabel: view.ProfileLabel,
					Source:       view.Source,
					Criteria:     virtualRankingCriteria(view.Criteria),
				}
				byListing[neutral] = ranking
			}
			ranked[file.ID] = ranking
		}
		if len(ranked) == 0 {
			return nil
		}
		return ranked
	}
}

// virtualRankingCriteria maps the virtual library's sort criteria to the
// catalog's wire-neutral projection.
func virtualRankingCriteria(criteria []quality.SortCriterion) []catalog.VirtualRankingCriterion {
	out := make([]catalog.VirtualRankingCriterion, 0, len(criteria))
	for _, criterion := range criteria {
		out = append(out, catalog.VirtualRankingCriterion{
			Attribute: criterion.Attribute,
			Direction: criterion.Direction,
		})
	}
	return out
}

// virtualNeutralSourcePath returns the shared virtual listing path of a
// content's candidate rows: the candidate URIs with their per-file result
// selector stripped, keeping any profile selector so the ranking uses the same
// profile the candidate list did. It returns "" when the files are not virtual.
func virtualNeutralSourcePath(files []*models.MediaFile) string {
	for _, file := range files {
		if file == nil || !strings.HasPrefix(file.FilePath, "virtual://") {
			continue
		}
		parsed, err := url.Parse(file.FilePath)
		if err != nil {
			continue
		}
		query := parsed.Query()
		query.Del("result")
		query.Del("results")
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	return ""
}
