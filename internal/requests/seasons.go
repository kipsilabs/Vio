package requests

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/metadata/tmdb"
)

// Season requests: a series request names the seasons it wants. A request
// made before season requests (no seasons) wants the whole series and keeps
// the old rule that any episode in the library fulfills it. A season is
// complete when every aired episode of it has a file in an enabled library
// (the library's own provider metadata dates the episodes, so no external
// service is involved); a season whose episodes have no air dates yet counts
// as complete once any of its episodes is present. A season dated to air
// later is not complete before its first episode airs, even if one arrived
// early.
//
// Once the download server reports a request done, the rule relaxes to the
// old one per season (see seasonDelivered): an episode the server could not
// find would otherwise hold the request, and its notification, open for good.

// SeasonCounts is one season's episodes: aired, dated to air later, and
// present in a library.
type SeasonCounts struct {
	Aired    int
	Upcoming int
	Have     int
}

// Complete reports whether the season is fully in the library.
func (c SeasonCounts) Complete() bool {
	if c.Aired > 0 {
		return c.Have >= c.Aired
	}
	return c.Upcoming == 0 && c.Have > 0
}

// SeasonPresenceResolver reports per-season episode counts for series in the
// library, by series content ID. CatalogPresence implements it.
type SeasonPresenceResolver interface {
	SeasonAvailability(ctx context.Context, seriesContentIDs []string) (map[string]map[int]SeasonCounts, error)
}

// SeasonProgress is one requested season's episode counts.
type SeasonProgress struct {
	Season int
	SeasonCounts
}

// RequestSeason is one season of a series as the detail page shows it.
type RequestSeason struct {
	Number       int          `json:"-"`
	Name         string       `json:"-"`
	EpisodeCount int          `json:"-"`
	AirDate      string       `json:"-"`
	PosterPath   string       `json:"-"`
	PosterURL    string       `json:"-"`
	Availability Availability `json:"-"`
	// Requested reports that the title's active request covers the season.
	Requested bool `json:"-"`
}

// AvailabilityPartial marks a season some, but not all, of whose aired
// episodes are in the library.
const AvailabilityPartial Availability = "partial"

// seasonCounts reads a series' per-season counts, or nil when the presence
// resolver cannot report seasons or the series is not in the library.
func (s *Service) seasonCounts(ctx context.Context, match PresenceMatch) (map[int]SeasonCounts, error) {
	resolver, ok := s.presence.(SeasonPresenceResolver)
	if !ok || !match.Available || match.ContentID == "" {
		return nil, nil
	}
	bySeries, err := resolver.SeasonAvailability(ctx, []string{match.ContentID})
	if err != nil {
		return nil, err
	}
	// A series in the library with no episode rows yet still has counts: none.
	if counts := bySeries[match.ContentID]; counts != nil {
		return counts, nil
	}
	return map[int]SeasonCounts{}, nil
}

// seasonProgress reports how far each requested season is, from the series'
// counts.
func seasonProgress(seasons []int, counts map[int]SeasonCounts) []SeasonProgress {
	out := make([]SeasonProgress, 0, len(seasons))
	for _, season := range seasons {
		out = append(out, SeasonProgress{Season: season, SeasonCounts: counts[season]})
	}
	return out
}

// seasonDelivered reports whether a requested season counts as in the
// library. A complete season always does; once the download server reports
// the request done, so does a season with any episode present. A season with
// no episode in the library never does: it may not have aired yet, and the
// server's word alone does not make it watchable.
func seasonDelivered(c SeasonCounts, serverDone bool) bool {
	return c.Complete() || (serverDone && c.Have > 0)
}

// seasonsDelivered reports whether every season in progress is delivered.
func seasonsDelivered(progress []SeasonProgress, serverDone bool) bool {
	for _, p := range progress {
		if !seasonDelivered(p.SeasonCounts, serverDone) {
			return false
		}
	}
	return len(progress) > 0
}

// requestFulfilled reports whether a request's media is in the library: a
// movie or a whole-series request when the title is, a season request when
// every requested season is complete. progress is set for season requests
// whose series is in the library.
func (s *Service) requestFulfilled(ctx context.Context, req Request, match PresenceMatch) (bool, []SeasonProgress, error) {
	if req.MediaType != MediaTypeSeries || len(req.Seasons) == 0 {
		return match.Available, nil, nil
	}
	if !requestSeasonCountsCompatible(req.SeasonSource, match) {
		return false, nil, nil
	}
	counts, err := s.seasonCounts(ctx, match)
	if err != nil || counts == nil {
		return false, nil, err
	}
	progress := seasonProgress(req.Seasons, counts)
	return seasonsDelivered(progress, req.Status == StatusCompleted), progress, nil
}

// Season source identifiers stored on a request and reported on detail.
const (
	SeasonSourceTVDB = "tvdb"
	SeasonSourceTMDB = "tmdb"
)

// SeriesSeason is one season of a series as the request detail and create
// paths see it, independent of which metadata source answered. PosterPath is a
// provider-relative path (TMDB); PosterURL is absolute artwork a provider such
// as TVDB supplies instead.
type SeriesSeason struct {
	Number       int
	Name         string
	EpisodeCount int
	AirDate      string
	PosterPath   string
	PosterURL    string
}

// seasonsFromTMDB converts TMDB's season list. TMDB groups long anime into a
// few wide seasons, which is why TVDB is preferred when a provider answers.
func seasonsFromTMDB(detail *tmdb.MediaDetail) []SeriesSeason {
	if detail == nil {
		return nil
	}
	out := make([]SeriesSeason, 0, len(detail.Seasons))
	for _, season := range detail.Seasons {
		out = append(out, SeriesSeason{
			Number: season.Number, Name: season.Name, EpisodeCount: season.EpisodeCount, AirDate: season.AirDate,
			PosterPath: season.PosterPath,
		})
	}
	return out
}

// regularSeasons drops specials. A source that lists only specials is not a
// usable series season list.
func regularSeasons(seasons []SeriesSeason) []SeriesSeason {
	return slices.DeleteFunc(slices.Clone(seasons), func(season SeriesSeason) bool { return season.Number <= 0 })
}

// seriesSeasonSource picks the season list a series request is checked and
// displayed against: the installed TVDB provider's when it answers, TMDB's
// otherwise. available reports whether any season list is available; a nil
// detail with no provider leaves the caller on the old whole-series rule.
//
// The provider is asked for English because the request TMDB client also
// returns English metadata (it sends no language).
func (s *Service) seriesSeasonSource(
	ctx context.Context,
	detail *tmdb.MediaDetail,
	tvdbID int,
	seasonSource string,
) (seasons []SeriesSeason, resolvedTVDBID int, fromTVDB bool, available bool, err error) {
	switch {
	case detail != nil && detail.TVDBID > 0:
		resolvedTVDBID = detail.TVDBID
	case tvdbID > 0:
		resolvedTVDBID = tvdbID
	}
	explicit := requestSeasonSource(seasonSource)
	// An explicit TMDB request bypasses TVDB entirely. This is the inverse of
	// the default path: the detail API and request validation must agree with
	// the source the requester chose.
	if explicit == SeasonSourceTMDB {
		return fallbackToTMDB(detail, resolvedTVDBID)
	}
	if resolvedTVDBID == 0 && detail != nil && s.tvdbResolver != nil {
		id, resolveErr := s.tvdbResolver.ResolveSeriesTVDBID(ctx, detail.ID, detail.IMDbID)
		switch {
		case resolveErr != nil && explicit == SeasonSourceTVDB:
			return nil, resolvedTVDBID, false, false,
				fmt.Errorf("%w: resolve TVDB id: %w", ErrSeasonSourceUnavailable, resolveErr)
		case resolveErr != nil:
			slog.DebugContext(ctx, "requests: resolve series tvdb id for seasons failed", "component", "requests",
				"tmdb_id", detail.ID, "err", resolveErr)
		case id > 0:
			resolvedTVDBID = id
		}
	}
	if resolvedTVDBID == 0 {
		if explicit == SeasonSourceTVDB {
			return nil, 0, false, false, fmt.Errorf("%w: no TVDB id", ErrSeasonSourceUnavailable)
		}
		return fallbackToTMDB(detail, resolvedTVDBID)
	}
	if s.seriesSeasonProvider == nil {
		if explicit == SeasonSourceTVDB {
			return nil, resolvedTVDBID, false, false,
				fmt.Errorf("%w: TVDB provider is not configured", ErrSeasonSourceUnavailable)
		}
		return fallbackToTMDB(detail, resolvedTVDBID)
	}
	results, fetchErr := s.seriesSeasonProvider.FetchSeriesTVDBSeasons(ctx, resolvedTVDBID, "en")
	if fetchErr != nil {
		if explicit == SeasonSourceTVDB {
			return nil, resolvedTVDBID, false, false,
				fmt.Errorf("%w: %w", ErrSeasonSourceUnavailable, fetchErr)
		}
		slog.DebugContext(ctx, "requests: fetch tvdb seasons failed; using tmdb", "component", "requests",
			"tvdb_id", resolvedTVDBID, "err", fetchErr)
		return fallbackToTMDB(detail, resolvedTVDBID)
	}
	if regular := regularSeasons(results); len(regular) > 0 {
		return regular, resolvedTVDBID, true, true, nil
	}
	if explicit == SeasonSourceTVDB {
		return nil, resolvedTVDBID, false, false,
			fmt.Errorf("%w: TVDB returned no regular seasons", ErrSeasonSourceUnavailable)
	}
	return fallbackToTMDB(detail, resolvedTVDBID)
}

func fallbackToTMDB(detail *tmdb.MediaDetail, resolvedTVDBID int) ([]SeriesSeason, int, bool, bool, error) {
	if detail != nil {
		return seasonsFromTMDB(detail), resolvedTVDBID, false, true, nil
	}
	return nil, resolvedTVDBID, false, false, nil
}

// seasonCountsCompatible reports whether catalog season counts use the same
// provider numbering as the request list. An integer season join across TVDB
// and TMDB is unsafe because the providers split and combine seasons
// differently, so unknown numbering provenance fails closed.
func seasonCountsCompatible(fromTVDB bool, match PresenceMatch) bool {
	catalogSource := catalogSeriesSeasonSource(match)
	if catalogSource == "" {
		return false
	}
	return (fromTVDB && catalogSource == SeasonSourceTVDB) ||
		(!fromTVDB && catalogSource == SeasonSourceTMDB)
}

// catalogSeriesSeasonSource reads numbering provenance from the catalog
// content ID. PresenceMatch.MatchedProvider records the external ID that found
// the item, which is not necessarily the provider whose seasons were indexed.
func catalogSeriesSeasonSource(match PresenceMatch) string {
	id := strings.ToLower(strings.TrimSpace(match.ContentID))
	switch {
	case strings.HasPrefix(id, "series-tvdb-"):
		return SeasonSourceTVDB
	case strings.HasPrefix(id, "series-tmdb-"):
		return SeasonSourceTMDB
	default:
		return ""
	}
}

// requestSeasonCountsCompatible gates a persisted request's season counts on
// the numbering source it was created with. Legacy requests without a frozen
// source keep the historical behavior.
func requestSeasonCountsCompatible(source string, match PresenceMatch) bool {
	source = requestSeasonSource(source)
	if source == "" {
		return true
	}
	return seasonCountsCompatible(source == SeasonSourceTVDB, match)
}

// airedSeasons returns the numbers of a series' regular seasons that have
// started airing, by the season metadata's dates.
func airedSeasons(seasons []SeriesSeason, today time.Time) []int {
	var out []int
	for _, season := range seasons {
		if season.Number <= 0 || season.AirDate == "" || season.EpisodeCount == 0 {
			continue
		}
		aired, err := time.Parse(time.DateOnly, season.AirDate)
		if err == nil && !aired.After(today) {
			out = append(out, season.Number)
		}
	}
	return out
}

// resolveRequestedSeasons decides which seasons a new series request asks
// for: the ones named, or every aired season, minus the seasons already
// complete in the library. It answers ErrAlreadyAvailable when nothing is left
// and ErrInvalidInput for a season the series metadata does not know. With no
// season list it returns the named seasons as they are (none means the whole
// series).
func (s *Service) resolveRequestedSeasons(ctx context.Context, named []int, seasons []SeriesSeason, available bool, match PresenceMatch, joinCounts bool) ([]int, error) {
	named = normalizeSeasons(named)
	if !available {
		if match.Available && len(named) == 0 {
			return nil, ErrAlreadyAvailable
		}
		return named, nil
	}
	known := map[int]bool{}
	for _, season := range seasons {
		if season.Number > 0 {
			known[season.Number] = true
		}
	}
	for _, season := range named {
		if !known[season] {
			return nil, fmt.Errorf("%w: season %d is not a season of this series", ErrInvalidInput, season)
		}
	}
	candidates := named
	if len(candidates) == 0 {
		candidates = airedSeasons(seasons, s.now())
	}
	var counts map[int]SeasonCounts
	if joinCounts {
		var err error
		counts, err = s.seasonCounts(ctx, match)
		if err != nil {
			return nil, err
		}
	}
	wanted := slices.DeleteFunc(slices.Clone(candidates), func(season int) bool {
		return counts[season].Complete()
	})
	if len(wanted) == 0 {
		if len(candidates) == 0 && !match.Available {
			// The series metadata lists no aired season yet: request the whole
			// series.
			return nil, nil
		}
		return nil, ErrAlreadyAvailable
	}
	return wanted, nil
}

func normalizeSeasons(seasons []int) []int {
	out := slices.DeleteFunc(slices.Clone(seasons), func(season int) bool { return season <= 0 })
	slices.Sort(out)
	return slices.Compact(out)
}

// requestSeasons builds the detail page's season list: the series metadata's
// regular seasons with their library availability and whether the active
// request covers them.
func requestSeasons(seasons []SeriesSeason, counts map[int]SeasonCounts, active *Request) []RequestSeason {
	var out []RequestSeason
	for _, season := range seasons {
		if season.Number <= 0 {
			continue
		}
		availability := AvailabilityMissing
		if c := counts[season.Number]; c.Complete() {
			availability = AvailabilityAvailable
		} else if c.Have > 0 {
			availability = AvailabilityPartial
		}
		requested := active != nil && (len(active.Seasons) == 0 || slices.Contains(active.Seasons, season.Number))
		out = append(out, RequestSeason{
			Number: season.Number, Name: season.Name, EpisodeCount: season.EpisodeCount, AirDate: season.AirDate,
			PosterPath: season.PosterPath, PosterURL: season.PosterURL,
			Availability: availability, Requested: requested,
		})
	}
	return out
}

// seriesHasOpenSeason reports whether a series in the library has a regular
// season it lacks: aired and incomplete, or not aired yet. The series detail
// then stays requestable, for the missing seasons or the upcoming ones.
func seriesHasOpenSeason(seasons []SeriesSeason, counts map[int]SeasonCounts) bool {
	for _, season := range seasons {
		if season.Number > 0 && !counts[season.Number].Complete() {
			return true
		}
	}
	return false
}
