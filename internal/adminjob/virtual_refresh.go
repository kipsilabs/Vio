package adminjob

import (
	"context"
	"time"
)

// JobTypeVirtualCandidatesRefresh is the asynchronous "Refresh List" job: it
// re-lists a virtual item's provider candidates, searches the indexers for
// releases the provider does not have, persists those indexer-only rows, and
// enriches the newly persisted candidates with probed track inventory.
const JobTypeVirtualCandidatesRefresh = "virtual_candidates_refresh"

// virtualCandidatesRefreshTimeout bounds one refresh job. The pipeline spends
// one provider re-list plus a bounded indexer search plus a bounded probe pass,
// so a few minutes is generous while still capping a hung provider.
const virtualCandidatesRefreshTimeout = 5 * time.Minute

// VirtualCandidatesRefreshRequest names the virtual item to refresh and carries
// the identity the background search needs. ContentID is the playable item (a
// movie or an episode) the watch detail exposes; EpisodeID is set to the same
// value for an episode and empty for a movie, so the persisted indexer-release
// scope matches the item the client reads. The remaining fields are the
// indexer-search inputs resolved once at acceptance time, so the job itself
// needs no catalog reads to build the query.
type VirtualCandidatesRefreshRequest struct {
	ContentID     string `json:"content_id"`
	EpisodeID     string `json:"episode_id,omitempty"`
	MediaFolderID int    `json:"media_folder_id"`

	// Owner identity for the provider re-list (plugin installation scope).
	UserID    int    `json:"user_id"`
	ProfileID string `json:"profile_id,omitempty"`

	// Indexer search identity.
	Title         string `json:"title,omitempty"`
	Year          int32  `json:"year,omitempty"`
	IMDbID        string `json:"imdb_id,omitempty"`
	TMDBID        string `json:"tmdb_id,omitempty"`
	TVDBID        string `json:"tvdb_id,omitempty"`
	SeasonNumber  int    `json:"season_number,omitempty"`
	EpisodeNumber int    `json:"episode_number,omitempty"`
}

// VirtualCandidatesRefreshResult summarizes what one refresh produced. It
// carries the retained watch-shape indexer release list so the accepted job
// echoes what every client will read next.
type VirtualCandidatesRefreshResult struct {
	ContentID          string `json:"content_id"`
	EpisodeID          string `json:"episode_id,omitempty"`
	ProviderCandidates int    `json:"provider_candidates"`
	IndexerReleases    int    `json:"indexer_releases"`
	Enriched           int    `json:"enriched"`
	// PrunedCandidates is how many dead, absent, unprotected candidate rows the
	// cleanup pass deleted. Purely informational.
	PrunedCandidates int `json:"pruned_candidates"`
	// PendingCandidates is how many freshly listed candidates AltMount
	// reports as actively fetching. Purely informational: these are the
	// releases playback will wait for rather than skip.
	PendingCandidates int                    `json:"pending_candidates"`
	IndexerSearchOK   bool                   `json:"indexer_search_ok"`
	Releases          []IndexerReleaseResult `json:"releases,omitempty"`
}

// IndexerReleaseResult is the safe, URL-free projection of one persisted
// indexer release carried on a refresh result. DownloadURL is deliberately
// absent: the URL is server-internal and never leaves the process.
type IndexerReleaseResult struct {
	ReleaseID     string     `json:"release_id"`
	Title         string     `json:"title"`
	Resolution    string     `json:"resolution,omitempty"`
	CodecVideo    string     `json:"codec_video,omitempty"`
	CodecAudio    string     `json:"codec_audio,omitempty"`
	HDR           bool       `json:"hdr,omitempty"`
	SizeBytes     int64      `json:"size_bytes,omitempty"`
	Indexer       string     `json:"indexer,omitempty"`
	PublishedAt   *time.Time `json:"published_at,omitempty"`
	FormatScore   *int       `json:"format_score,omitempty"`
	Protocol      string     `json:"protocol,omitempty"`
	DownloadState string     `json:"download_state"`
}

// VirtualCandidatesRefreshExecutor runs one refresh job. It is implemented by
// the HTTP layer (which owns the provider listing, probe and event wiring) and
// injected into the runner, mirroring the item- and library-refresh executors.
type VirtualCandidatesRefreshExecutor interface {
	Execute(ctx context.Context, req VirtualCandidatesRefreshRequest, progress func(current, total int, message string)) (*VirtualCandidatesRefreshResult, error)
}

// SetVirtualRefreshExecutor installs the refresh executor. It is optional: a
// runner without one fails the job with a clear message instead of hanging.
func (r *Runner) SetVirtualRefreshExecutor(executor VirtualCandidatesRefreshExecutor) {
	if r != nil {
		r.virtualRefresh = executor
	}
}
